// Package hlskey fetches and caches HLS AES-128 content keys, and decrypts
// segments encrypted with them.
//
// It serves both of the gateway's encryption modes: in decrypt-on-ingest mode
// the processor uses Decrypt to store cleartext, and in passthrough mode the
// key proxy uses the same cache to serve keys to players without ever exposing
// the upstream key URL.
package hlskey

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/fetch"
)

// KeyLength is the size of an HLS AES-128 content key.
const KeyLength = 16

// KeyID returns the stable identifier the gateway publishes for an upstream key
// URI. It is the hex SHA-256 of the URI.
//
// Publishing a hash rather than the URI itself is what keeps the key proxy from
// becoming an open relay: the proxy resolves an ID through a registry of URIs
// actually seen in this channel's playlists, so it can never be talked into
// fetching an arbitrary URL.
func KeyID(uri string) string {
	sum := sha256.Sum256([]byte(uri))
	return hex.EncodeToString(sum[:])
}

// Cache fetches content keys over HTTP and remembers them.
//
// Keys are small, immutable, and referenced by every segment in a key period,
// so they are cached for the process lifetime. Concurrent requests for the same
// key are collapsed into one upstream fetch — segments are downloaded in
// parallel, and without this a key rotation would burst N simultaneous requests
// at the key server.
type Cache struct {
	client *http.Client
	// headers is shared with the channel's fetcher and watcher, so rotating the
	// channel's upstream credentials reaches key requests too. Note this only
	// affects key URIs not yet fetched: a key already in entries is never
	// re-requested (see the caching note above), which is the intended
	// behaviour — the key itself does not expire, only the credential does.
	headers fetch.Headers
	// storePath, when set, persists fetched keys to disk so a passthrough
	// channel can keep serving them after a restart even if the upstream key
	// server has gone away. Empty means memory-only.
	storePath string

	mu      sync.Mutex
	entries map[string]*entry
	// stored mirrors the on-disk key file (uri → hex key) so persisting a new
	// key is a single whole-file rewrite rather than a read-modify-write.
	stored map[string]string
}

// entry is one in-flight or completed key fetch.
type entry struct {
	done chan struct{}
	key  []byte
	err  error
}

// NewCache creates a Cache. A nil client gets a default with a short timeout;
// headers are applied to every key request so token-gated key servers work.
//
// storePath, when non-empty, is a JSON file the cache persists keys to and
// reloads on construction. Pass it only for passthrough channels: it trades
// at-rest protection (the key sits on disk beside the ciphertext) for the
// ability to serve keys after the upstream key server is gone. Decrypt-mode
// channels do not need it and should pass "".
func NewCache(client *http.Client, headers fetch.Headers, storePath string) *Cache {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	c := &Cache{
		client:    client,
		headers:   headers,
		storePath: storePath,
		entries:   make(map[string]*entry),
		stored:    make(map[string]string),
	}
	c.loadStore()
	return c
}

// Get returns the content key at uri, fetching it at most once.
func (c *Cache) Get(ctx context.Context, uri string) ([]byte, error) {
	c.mu.Lock()
	e, ok := c.entries[uri]
	if ok {
		c.mu.Unlock()
		select {
		case <-e.done:
			return e.key, e.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e = &entry{done: make(chan struct{})}
	c.entries[uri] = e
	c.mu.Unlock()

	key, err := c.fetch(ctx, uri)
	e.key, e.err = key, err
	close(e.done)

	// A failure must not be remembered as an answer, or a transient key-server
	// outage would poison the channel until restart.
	if err != nil {
		c.mu.Lock()
		delete(c.entries, uri)
		c.mu.Unlock()
	} else {
		c.persist(uri, key)
	}
	return key, err
}

func (c *Cache) fetch(ctx context.Context, uri string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	fetch.ApplyHeaders(req, c.headers)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key server status %d for %s", resp.StatusCode, uri)
	}

	// Bound the read: a content key is 16 bytes, and an origin misconfigured to
	// return a large body should not be streamed into memory.
	key, err := io.ReadAll(io.LimitReader(resp.Body, KeyLength+1))
	if err != nil {
		return nil, err
	}
	if len(key) != KeyLength {
		return nil, fmt.Errorf("key from %s is %d bytes, want %d", uri, len(key), KeyLength)
	}
	return key, nil
}

// Decrypt decrypts an AES-128-CBC encrypted HLS segment and removes its PKCS#7
// padding (RFC 8216 §5.2).
func Decrypt(data, key, iv []byte) ([]byte, error) {
	if len(key) != KeyLength {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(key), KeyLength)
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("IV is %d bytes, want %d", len(iv), aes.BlockSize)
	}
	if len(data) == 0 {
		return nil, errors.New("empty ciphertext")
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of the AES block size", len(data))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)

	return unpadPKCS7(out)
}

// unpadPKCS7 strips and validates PKCS#7 padding.
func unpadPKCS7(b []byte) ([]byte, error) {
	n := len(b)
	if n == 0 {
		return nil, errors.New("no data after decryption")
	}
	pad := int(b[n-1])
	if pad == 0 || pad > aes.BlockSize || pad > n {
		return nil, fmt.Errorf("invalid PKCS#7 padding length %d", pad)
	}
	// Compare in constant time: padding validity is derived from decrypted
	// bytes, and a data-dependent early exit here is the classic CBC padding
	// oracle.
	want := make([]byte, pad)
	for i := range want {
		want[i] = byte(pad)
	}
	if subtle.ConstantTimeCompare(b[n-pad:], want) != 1 {
		return nil, errors.New("invalid PKCS#7 padding")
	}
	return b[:n-pad], nil
}

// KeyStorePath returns the on-disk key file for a channel.
func KeyStorePath(dataDir, channelID string) string {
	return filepath.Join(dataDir, channelID, "keys.json")
}

// loadStore reads any persisted keys into memory as completed cache entries, so
// they are served without contacting the upstream. A missing or corrupt file is
// ignored: the cache simply falls back to fetching.
func (c *Cache) loadStore() {
	if c.storePath == "" {
		return
	}
	data, err := os.ReadFile(c.storePath)
	if err != nil {
		return
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for uri, hexKey := range m {
		key, err := hex.DecodeString(hexKey)
		if err != nil || len(key) != KeyLength {
			continue
		}
		c.stored[uri] = hexKey
		e := &entry{done: make(chan struct{}), key: key}
		close(e.done)
		c.entries[uri] = e
	}
}

// persist records a fetched key in the on-disk store. Best-effort: a write
// failure only costs restart recovery, not live serving. The key material is
// written 0600 since it can decrypt the co-located segments.
func (c *Cache) persist(uri string, key []byte) {
	if c.storePath == "" {
		return
	}
	c.mu.Lock()
	if c.stored[uri] == hex.EncodeToString(key) {
		c.mu.Unlock()
		return // already persisted
	}
	c.stored[uri] = hex.EncodeToString(key)
	snapshot := make(map[string]string, len(c.stored))
	for k, v := range c.stored {
		snapshot[k] = v
	}
	c.mu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.storePath), 0o755); err != nil {
		return
	}
	// Write atomically so a crash cannot leave a half-written key file. The temp
	// file gets a unique name: warmKey runs one goroutine per key, so two
	// persists overlap routinely, and a shared name would let them write over
	// each other's bytes between write and rename.
	tmp, err := os.CreateTemp(filepath.Dir(c.storePath), filepath.Base(c.storePath)+".*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	// The key file must never be group- or world-readable, and CreateTemp makes
	// it 0600 already; set it explicitly so the guarantee is local and obvious.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, c.storePath); err != nil {
		os.Remove(tmpName)
	}
}
