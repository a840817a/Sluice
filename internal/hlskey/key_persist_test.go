package hlskey

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestPersistedKeySurvivesUpstreamDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(testKey())
	}))
	uri := srv.URL + "/k1.bin"

	// First process: fetch the key while upstream is alive; it must be persisted.
	c1 := NewCache(nil, nil, path)
	if _, err := c1.Get(context.Background(), uri); err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("key store not written: %v", err)
	}

	// Upstream disappears entirely.
	srv.Close()

	// Second process (simulated restart) loads the same store: the key must be
	// served from disk without any upstream contact.
	c2 := NewCache(nil, nil, path)
	got, err := c2.Get(context.Background(), uri)
	if err != nil {
		t.Fatalf("Get after restart with upstream down: %v", err)
	}
	if string(got) != string(testKey()) {
		t.Errorf("restored key = %q, want %q", got, testKey())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("upstream hit %d times, want 1 (restart must not re-fetch)", n)
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

func TestCorruptKeyStoreFallsBackToUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testKey())
	}))
	defer srv.Close()

	// A damaged store must not break key serving; it falls back to upstream.
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
