// Package hlskey fetches and caches HLS AES-128 content keys, and decrypts
// segments encrypted with them.
//
// It serves both of the gateway's encryption modes: in decrypt-on-ingest mode
// the processor uses Decrypt to store cleartext, and in passthrough mode the
// key proxy uses the same cache to serve keys to players without ever exposing
// the key server's URL.
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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/store"
)

// KeyLength is the size of an HLS AES-128 content key.
const KeyLength = 16

// KeyID returns the stable identifier the gateway publishes for a key server
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
// so they are cached for the process lifetime. Concurrent requests for the
// same key are collapsed into one fetch to the key server — segments are
// downloaded in parallel, and without this a key rotation would burst N
// simultaneous requests at the key server.
type Cache struct {
	client *http.Client
	// origin shares its headers with the channel's fetcher and watcher, so
	// rotating the channel's origin credentials reaches key requests too. Note this only
	// affects key URIs not yet fetched: a key already in entries is never
	// re-requested (see the caching note above), which is the intended
	// behaviour — the key itself does not expire, only the credential does.
	origin *fetch.Origin
	// storePath, when set, persists fetched keys to disk so a passthrough
	// channel can keep serving them after a restart even if the key server
	// has gone away. Empty means memory-only.
	storePath string

	mu      sync.Mutex
	entries map[string]*entry
	// stored is the desired on-disk key file (uri → hex key). It is retained
	// after a best-effort write failure so a required checkpoint can retry it.
	stored map[string]string
	// persistMu serializes whole-file snapshots and writes. Without it, two
	// concurrent persists can each write an older snapshot after the other has
	// included a newer key, dropping that key from disk.
	persistMu sync.Mutex
}

// entry is one in-flight or completed key fetch.
type entry struct {
	done chan struct{}
	key  []byte
	err  error
}

// NewCache creates a Cache. A nil client gets a default with a short timeout;
// every key request goes through origin, so token-gated key servers work.
//
// storePath, when non-empty, is a JSON file the cache persists keys to and
// reloads on construction. Pass it only for passthrough channels: it trades
// at-rest protection (the key sits on disk beside the ciphertext) for the
// ability to serve keys after the key server is gone. Decrypt-mode channels
// do not need it and should pass "".
func NewCache(client *http.Client, origin *fetch.Origin, storePath string) *Cache {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	c := &Cache{
		client:    client,
		origin:    origin,
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
		c.persist()
	}
	return key, err
}

func (c *Cache) fetch(ctx context.Context, uri string) ([]byte, error) {
	// Bound the read: a content key is 16 bytes, and an origin misconfigured to
	// return a large body should not be streamed into memory.
	key, err := c.origin.Get(ctx, c.client, uri, KeyLength+1)
	if se := (*fetch.StatusError)(nil); errors.As(err, &se) {
		return nil, fmt.Errorf("key server status %d for %s", se.Code, uri)
	}
	if errors.Is(err, fetch.ErrTooLarge) {
		return nil, fmt.Errorf("key from %s is %d bytes, want %d", uri, KeyLength+1, KeyLength)
	}
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

// loadStore reads any persisted keys into memory as completed cache entries,
// so they are served without contacting the key server. A missing or corrupt
// file is ignored: the cache simply falls back to fetching.
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

// Checkpoint fetches every required key and synchronously writes every
// completed successful key to disk. It is used when a VOD transition needs a
// restart-safe key proxy, rather than the best-effort persistence used during
// live ingest.
func (c *Cache) Checkpoint(ctx context.Context, requiredURIs []string) error {
	if len(requiredURIs) == 0 {
		return nil
	}
	if c.storePath == "" {
		return errors.New("HLS key store path is not configured")
	}
	for _, uri := range requiredURIs {
		if _, err := c.Get(ctx, uri); err != nil {
			return fmt.Errorf("fetch required HLS key %q: %w", uri, err)
		}
	}
	if err := c.writeStore(true); err != nil {
		return fmt.Errorf("durably write HLS key store: %w", err)
	}
	return nil
}

// persist records a fetched key in the on-disk store. Best-effort: a write
// failure only costs restart recovery, not live serving. It runs on the path
// of a player's first key request, so it does not fsync; Checkpoint writes the
// durable copy, retrying every completed key, including one whose persist failed.
func (c *Cache) persist() {
	if c.storePath == "" {
		return
	}
	_ = c.writeStore(false)
}

// writeStore serializes the current completed keys and writes a whole-file
// snapshot, durably (file and directory fsync) when durable is set. persistMu
// covers both snapshot and write so an older snapshot can never rename over a
// newer one.
func (c *Cache) writeStore(durable bool) error {
	c.persistMu.Lock()
	defer c.persistMu.Unlock()

	c.mu.Lock()
	snapshot := make(map[string]string, len(c.stored)+len(c.entries))
	for uri, hexKey := range c.stored {
		snapshot[uri] = hexKey
	}
	for uri, e := range c.entries {
		select {
		case <-e.done:
			if e.err == nil && len(e.key) == KeyLength {
				snapshot[uri] = hex.EncodeToString(e.key)
			}
		default:
		}
	}
	for uri, hexKey := range snapshot {
		c.stored[uri] = hexKey
	}
	c.mu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if durable {
		return store.WriteDurable(c.storePath, data, 0o600)
	}
	return writeAtomic(c.storePath, data)
}

// writeAtomic replaces path without fsync. The key file must never be group-
// or world-readable, so the temp file is made 0600 explicitly before rename.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmp.Name(), path, err)
	}
	return nil
}

// Cached returns a copy of an already acquired key without contacting the origin.
// Local exports call it after the VOD key checkpoint has finished.
func (c *Cache) Cached(uri string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("HLS key cache unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[uri]; e != nil {
		select {
		case <-e.done:
			if e.err == nil && len(e.key) == KeyLength {
				return append([]byte(nil), e.key...), nil
			}
		default:
		}
	}
	return nil, fmt.Errorf("HLS key is not cached: %s", KeyID(uri))
}
