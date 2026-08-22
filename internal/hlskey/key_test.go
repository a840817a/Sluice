package hlskey

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/a840817a/sluice/internal/fetch"
)

// encryptAES128 is the inverse of Decrypt, used to build fixtures.
func encryptAES128(t *testing.T, key, iv, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	// PKCS#7 pad.
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)

	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

func testKey() []byte { return []byte("0123456789abcdef") }
func testIV() []byte  { return []byte("fedcba9876543210") }

func TestDecryptRoundTrip(t *testing.T) {
	// A payload that is not a block multiple, so padding actually matters.
	plaintext := []byte("this is a fake TS payload of awkward length")
	ct := encryptAES128(t, testKey(), testIV(), plaintext)

	got, err := Decrypt(ct, testKey(), testIV())
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

func TestDecryptExactBlockMultiple(t *testing.T) {
	// Exactly one block of plaintext still gets a full block of padding.
	plaintext := []byte("sixteen bytes!!!")
	if len(plaintext) != aes.BlockSize {
		t.Fatalf("fixture is %d bytes, want %d", len(plaintext), aes.BlockSize)
	}
	ct := encryptAES128(t, testKey(), testIV(), plaintext)
	if len(ct) != 2*aes.BlockSize {
		t.Fatalf("ciphertext is %d bytes, want a full extra padding block", len(ct))
	}

	got, err := Decrypt(ct, testKey(), testIV())
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

func TestDecryptRejectsBadInput(t *testing.T) {
	ct := encryptAES128(t, testKey(), testIV(), []byte("hello"))

	tests := []struct {
		name    string
		data    []byte
		key, iv []byte
	}{
		{"short key", ct, []byte("tooshort"), testIV()},
		{"short IV", ct, testKey(), []byte("bad")},
		{"not a block multiple", ct[:len(ct)-1], testKey(), testIV()},
		{"empty", nil, testKey(), testIV()},
	}
	for _, tc := range tests {
		if _, err := Decrypt(tc.data, tc.key, tc.iv); err == nil {
			t.Errorf("Decrypt(%s) = nil error, want failure", tc.name)
		}
	}
}

func TestDecryptRejectsCorruptPadding(t *testing.T) {
	// Flipping bytes in the final block corrupts the PKCS#7 padding. This must
	// be an error rather than silently returning garbage of the wrong length.
	ct := encryptAES128(t, testKey(), testIV(), []byte("some payload"))
	ct[len(ct)-1] ^= 0xFF

	if _, err := Decrypt(ct, testKey(), testIV()); err == nil {
		t.Error("Decrypt accepted corrupt padding")
	}
}

func TestCacheFetchesOnceAndReuses(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(testKey())
	}))
	defer srv.Close()

	c := NewCache(nil, nil, "")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := c.Get(ctx, srv.URL+"/k1.bin")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(got, testKey()) {
			t.Fatalf("key = %q, want %q", got, testKey())
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("key server hit %d times, want 1 (cache not reused)", n)
	}
}

func TestCacheSingleFlight(t *testing.T) {
	// Many segments referencing one key arrive at once. The key server must not
	// be stampeded — segments are fetched concurrently by the worker pool.
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // hold the request open so all callers pile up
		w.Write(testKey())
	}))
	defer srv.Close()

	c := NewCache(nil, nil, "")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err != nil {
				errs <- err
			}
		}()
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Get: %v", err)
	}

	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("key server hit %d times under concurrency, want 1", n)
	}
}

func TestCacheSendsConfiguredHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Token")
		w.Write(testKey())
	}))
	defer srv.Close()

	c := NewCache(nil, fetch.NewAtomicHeaders(map[string]string{"X-Token": "secret"}), "")
	if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "secret" {
		t.Errorf("X-Token = %q, want the configured value", got)
	}
}

// A channel's upstream credentials can be rotated while it is running, so the
// key cache must read the provider per request rather than copying it at
// construction. Only key URIs not yet fetched are affected: an already-cached
// key is never re-requested.
func TestCacheUsesRotatedHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Token")
		w.Write(testKey())
	}))
	defer srv.Close()

	h := fetch.NewAtomicHeaders(map[string]string{"X-Token": "old"})
	c := NewCache(nil, h, "")
	if _, err := c.Get(context.Background(), srv.URL+"/k1.bin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "old" {
		t.Fatalf("X-Token = %q, want %q before rotation", got, "old")
	}

	h.Store(map[string]string{"X-Token": "new"})
	if _, err := c.Get(context.Background(), srv.URL+"/k2.bin"); err != nil {
		t.Fatalf("Get after rotation: %v", err)
	}
	if got != "new" {
		t.Errorf("X-Token = %q after rotation, want %q", got, "new")
	}
}

func TestCacheRejectsBadKeyLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not sixteen"))
	}))
	defer srv.Close()

	c := NewCache(nil, nil, "")
	if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err == nil {
		t.Error("Get accepted a key that is not 16 bytes")
	}
}

func TestCachePropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewCache(nil, nil, "")
	if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err == nil {
		t.Error("Get ignored a 403 response")
	}
	// A failed fetch must not be cached as a success.
	if _, err := c.Get(context.Background(), srv.URL+"/k.bin"); err == nil {
		t.Error("second Get returned success after a failure was cached")
	}
}

func TestKeyID(t *testing.T) {
	// Stable, collision-resistant, and never reveals the upstream URI.
	a := KeyID("https://drm.example.com/keys/k1.bin")
	b := KeyID("https://drm.example.com/keys/k1.bin")
	c := KeyID("https://drm.example.com/keys/k2.bin")

	if a != b {
		t.Error("KeyID is not stable for the same URI")
	}
	if a == c {
		t.Error("KeyID collided for different URIs")
	}
	if len(a) != 64 {
		t.Errorf("KeyID length = %d, want 64 hex chars", len(a))
	}
	if bytes.Contains([]byte(a), []byte("example.com")) {
		t.Error("KeyID leaks the upstream URI")
	}
}
