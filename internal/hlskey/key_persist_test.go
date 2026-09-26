package hlskey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPersistedKeySurvivesKeyServerDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(testKey())
	}))
	uri := srv.URL + "/k1.bin"

	// First process: fetch the key while the key server is alive; it must
	// be persisted.
	c1 := NewCache(nil, nil, path)
	if _, err := c1.Get(context.Background(), uri); err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("key store not written: %v", err)
	}

	// The key server disappears entirely.
	srv.Close()

	// Second process (simulated restart) loads the same store: the key must be
	// served from disk without any key server contact.
	c2 := NewCache(nil, nil, path)
	got, err := c2.Get(context.Background(), uri)
	if err != nil {
		t.Fatalf("Get after restart with the key server down: %v", err)
	}
	if string(got) != string(testKey()) {
		t.Errorf("restored key = %q, want %q", got, testKey())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("key server hit %d times, want 1 (restart must not re-fetch)", n)
	}
}

func TestMemoryOnlyCacheDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testKey())
	}))
	defer srv.Close()

	// No store path: nothing must be written to disk.
	c := NewCache(nil, nil, "")
	if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("memory-only cache wrote %d files to disk", len(entries))
	}
}

func TestKeyStoreFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testKey())
	}))
	defer srv.Close()

	c := NewCache(nil, nil, path)
	if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Key material must not be readable by other users.
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("key store perms = %o, want no group/other access", info.Mode().Perm())
	}
}

func TestCorruptKeyStoreFallsBackToKeyServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testKey())
	}))
	defer srv.Close()

	// A damaged store must not break key serving; it falls back to the key server.
	c := NewCache(nil, nil, path)
	got, err := c.Get(context.Background(), srv.URL+"/k.bin")
	if err != nil {
		t.Fatalf("Get with corrupt store: %v", err)
	}
	if string(got) != string(testKey()) {
		t.Errorf("key = %q, want %q", got, testKey())
	}
}

func TestPersistedStoreAccumulatesMultipleKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testKey())
	}))
	defer srv.Close()

	c := NewCache(nil, nil, path)
	for _, name := range []string{"/k1.bin", "/k2.bin", "/k3.bin"} {
		if _, err := c.Get(context.Background(), srv.URL+name); err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
	}
	srv.Close()

	// All three must reload after a restart, not just the last one written.
	c2 := NewCache(nil, nil, path)
	for _, name := range []string{"/k1.bin", "/k2.bin", "/k3.bin"} {
		if _, err := c2.Get(context.Background(), srv.URL+name); err != nil {
			t.Errorf("key %s not persisted: %v", name, err)
		}
	}
}

func TestCheckpointFetchesRequiredKeysAndWritesAllSuccessfulKeys(t *testing.T) {
	dir := t.TempDir()
	blockedParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testKey())
	}))
	defer srv.Close()

	uri := srv.URL + "/key.bin"
	// The live, best-effort persist fails because its parent is a file. The
	// successful in-memory entry must still be captured by the later required
	// checkpoint after its destination is fixed.
	c := NewCache(nil, nil, filepath.Join(blockedParent, "keys.json"))
	if _, err := c.Get(context.Background(), uri); err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.storePath = filepath.Join(dir, "keys.json")
	if err := c.Checkpoint(context.Background(), []string{uri}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	data, err := os.ReadFile(c.storePath)
	if err != nil {
		t.Fatalf("read key checkpoint: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode key checkpoint: %v", err)
	}
	if got[uri] == "" {
		t.Errorf("checkpoint omitted successful in-memory key: %v", got)
	}
}

func TestCheckpointRetriesFailedRequiredKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(testKey())
	}))
	defer srv.Close()

	c := NewCache(nil, nil, path)
	uri := srv.URL + "/key.bin"
	if _, err := c.Get(context.Background(), uri); err == nil {
		t.Fatal("initial transient Get succeeded")
	}
	if err := c.Checkpoint(context.Background(), []string{uri}); err != nil {
		t.Fatalf("Checkpoint after transient failure: %v", err)
	}
	if hits := atomic.LoadInt32(&hits); hits != 2 {
		t.Errorf("key server hits = %d, want 2", hits)
	}
}

func TestCheckpointRequiresKeyStoreOnlyWhenKeysAreRequired(t *testing.T) {
	c := NewCache(nil, nil, "")
	if err := c.Checkpoint(context.Background(), nil); err != nil {
		t.Fatalf("Checkpoint with no required keys: %v", err)
	}
	// A reachable key server, so the missing store path is the only thing
	// that can make the checkpoint fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testKey())
	}))
	defer srv.Close()
	err := c.Checkpoint(context.Background(), []string{srv.URL + "/key.bin"})
	if err == nil || !strings.Contains(err.Error(), "store path is not configured") {
		t.Errorf("Checkpoint with a required key and no store path = %v, want the missing store path reported", err)
	}
}
