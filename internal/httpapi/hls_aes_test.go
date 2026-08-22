package httpapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/hlskey"
)

var aesTestKey = []byte("0123456789abcdef")

// encryptSegment AES-128-CBC encrypts with PKCS#7 padding, as an HLS packager does.
func encryptSegment(t *testing.T, plaintext, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(aesTestKey)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

// seqIV is the IV an HLS client derives when EXT-X-KEY omits one.
func seqIV(seqNo uint64) []byte {
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], seqNo)
	return iv
}

// startEncryptedOrigin serves an AES-128 encrypted HLS stream. When ivInTag is
// false the playlist omits IV, forcing clients to derive it from the sequence
// number. keyHits counts requests to the key endpoint.
func startEncryptedOrigin(t *testing.T, segCount int, ivInTag bool, keyHits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nmedia/v0.m3u8\n")
	})
	mux.HandleFunc("/key.bin", func(w http.ResponseWriter, r *http.Request) {
		if keyHits != nil {
			atomic.AddInt32(keyHits, 1)
		}
		w.Write(aesTestKey)
	})
	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		keyLine := `#EXT-X-KEY:METHOD=AES-128,URI="../key.bin"`
		if ivInTag {
			keyLine += ",IV=0x000102030405060708090a0b0c0d0e0f"
		}
		b.WriteString(keyLine + "\n")
		for i := 0; i < segCount; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.000,\nseg%d.ts\n", i)
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/seg%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		iv := seqIV(uint64(n))
		if ivInTag {
			iv = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
		}
		w.Write(encryptSegment(t, tsSegment(n), iv))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func startChannelWithKeyMode(t *testing.T, originURL, keyMode string) *Server {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "enc", ch.Config{
		SourceType: ch.SourceHLS,
		HLSKeyMode: keyMode,
		MPDURL:     originURL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("enc") })
	return NewServer(cfg, mgr)
}

func waitForEncPlaylist(t *testing.T, srv *Server, want int) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
			"/v1/channels/enc/media/v0.m3u8",
			map[string]string{"channelID": "enc", "repID": "v0"})
		body = w.Body.String()
		if w.Code == http.StatusOK && strings.Count(body, "#EXTINF:") >= want {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d segments; last playlist:\n%s", want, body)
	return ""
}

func fetchSegment(t *testing.T, srv *Server, n int) []byte {
	t.Helper()
	segFile := fmt.Sprintf("%d.ts", n)
	w := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/enc/segments/v0/"+segFile,
		map[string]string{"channelID": "enc", "repID": "v0", "segFile": segFile})
	if w.Code != http.StatusOK {
		t.Fatalf("segment %s: got %d", segFile, w.Code)
	}
	return w.Body.Bytes()
}

// TestAES128DecryptMode: the gateway decrypts during ingest and republishes in
// the clear, so stored segments are real TS and the playlist carries no key.
func TestAES128DecryptMode(t *testing.T) {
	origin := startEncryptedOrigin(t, 3, true, nil)
	srv := startChannelWithKeyMode(t, origin.URL, ch.KeyModeDecrypt)

	body := waitForEncPlaylist(t, srv, 3)
	if strings.Contains(body, "EXT-X-KEY") {
		t.Errorf("decrypt-mode playlist must not advertise a key:\n%s", body)
	}

	got := fetchSegment(t, srv, 0)
	if !bytes.Equal(got, tsSegment(0)) {
		t.Error("decrypted segment does not match the original plaintext")
	}
	// Cleartext TS starts with the sync byte; ciphertext almost never would.
	if got[0] != 0x47 {
		t.Errorf("stored segment is not cleartext TS (first byte 0x%02x)", got[0])
	}
}

// TestAES128PassthroughMode: segments stay encrypted on disk and the playlist
// points at the gateway's key proxy rather than the upstream key server.
func TestAES128PassthroughMode(t *testing.T) {
	origin := startEncryptedOrigin(t, 3, true, nil)
	srv := startChannelWithKeyMode(t, origin.URL, ch.KeyModePassthrough)

	body := waitForEncPlaylist(t, srv, 3)
	if !strings.Contains(body, "#EXT-X-KEY:METHOD=AES-128") {
		t.Fatalf("passthrough playlist must advertise a key:\n%s", body)
	}
	if strings.Contains(body, origin.URL) {
		t.Errorf("playlist leaks the upstream key URL:\n%s", body)
	}
	if !strings.Contains(body, "http://gw.test/v1/channels/enc/key/") {
		t.Errorf("key URI does not point at the gateway proxy:\n%s", body)
	}
	// The IV must be explicit, never left for the client to derive from our
	// renumbered media sequence.
	if !strings.Contains(body, "IV=0x") {
		t.Errorf("playlist omits an explicit IV:\n%s", body)
	}

	// Stored bytes are still ciphertext.
	got := fetchSegment(t, srv, 0)
	if bytes.Equal(got, tsSegment(0)) {
		t.Error("passthrough stored cleartext; segment should still be encrypted")
	}

	// The advertised key, fetched through the proxy, must actually decrypt it.
	keyID := hlskey.KeyID(origin.URL + "/key.bin")
	kw := executeRequest(srv.handleKeyProxy, http.MethodGet,
		"/v1/channels/enc/key/"+keyID,
		map[string]string{"channelID": "enc", "keyID": keyID})
	if kw.Code != http.StatusOK {
		t.Fatalf("key proxy: got %d: %s", kw.Code, kw.Body.String())
	}
	if kw.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("key response Cache-Control = %q, want no-store", kw.Header().Get("Cache-Control"))
	}

	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	plain, err := hlskey.Decrypt(got, kw.Body.Bytes(), iv)
	if err != nil {
		t.Fatalf("decrypt with proxied key: %v", err)
	}
	if !bytes.Equal(plain, tsSegment(0)) {
		t.Error("proxied key did not correctly decrypt the served segment")
	}
}

// TestAES128DerivedIVWhenTagOmitsIt covers the case the JS reference project
// got wrong: no IV attribute, so it comes from the media sequence number.
func TestAES128DerivedIVWhenTagOmitsIt(t *testing.T) {
	origin := startEncryptedOrigin(t, 3, false, nil)
	srv := startChannelWithKeyMode(t, origin.URL, ch.KeyModeDecrypt)

	waitForEncPlaylist(t, srv, 3)
	for n := 0; n < 3; n++ {
		if got := fetchSegment(t, srv, n); !bytes.Equal(got, tsSegment(n)) {
			t.Errorf("segment %d decrypted incorrectly with a derived IV", n)
		}
	}
}

// TestKeyProxyRejectsUnknownKeyID is the SSRF guard: only key URIs seen in this
// channel's own playlists are resolvable.
func TestKeyProxyRejectsUnknownKeyID(t *testing.T) {
	origin := startEncryptedOrigin(t, 2, true, nil)
	srv := startChannelWithKeyMode(t, origin.URL, ch.KeyModePassthrough)
	waitForEncPlaylist(t, srv, 2)

	evil := hlskey.KeyID("http://169.254.169.254/latest/meta-data/")
	w := executeRequest(srv.handleKeyProxy, http.MethodGet,
		"/v1/channels/enc/key/"+evil,
		map[string]string{"channelID": "enc", "keyID": evil})
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown key ID: got %d, want 404", w.Code)
	}
}

// TestKeyProxyDisabledInDecryptMode: content is already republished in the
// clear, so handing out the upstream key would leak it for nothing.
func TestKeyProxyDisabledInDecryptMode(t *testing.T) {
	origin := startEncryptedOrigin(t, 2, true, nil)
	srv := startChannelWithKeyMode(t, origin.URL, ch.KeyModeDecrypt)
	waitForEncPlaylist(t, srv, 2)

	keyID := hlskey.KeyID(origin.URL + "/key.bin")
	w := executeRequest(srv.handleKeyProxy, http.MethodGet,
		"/v1/channels/enc/key/"+keyID,
		map[string]string{"channelID": "enc", "keyID": keyID})
	if w.Code != http.StatusNotFound {
		t.Errorf("key proxy in decrypt mode: got %d, want 404", w.Code)
	}
}

// TestKeyFetchedOnceAcrossManySegments guards the key server against a
// stampede from the parallel segment workers.
func TestKeyFetchedOnceAcrossManySegments(t *testing.T) {
	var keyHits int32
	origin := startEncryptedOrigin(t, 8, true, &keyHits)
	srv := startChannelWithKeyMode(t, origin.URL, ch.KeyModeDecrypt)

	waitForEncPlaylist(t, srv, 8)
	if n := atomic.LoadInt32(&keyHits); n != 1 {
		t.Errorf("key server hit %d times for 8 segments, want 1", n)
	}
}

// startEncryptedOriginWithData is like startEncryptedOrigin but takes a shared
// dataDir so a channel's segments/keys persist across a simulated restart.
func encChannelConfig(originURL string) ch.Config {
	return ch.Config{
		SourceType: ch.SourceHLS,
		HLSKeyMode: ch.KeyModePassthrough,
		MPDURL:     originURL + "/master.m3u8",
		Enabled:    true,
	}
}

// TestPassthroughKeySurvivesUpstreamDownRestart is the acceptance test for
// gateway-side key persistence: after the origin (and its key server) is gone,
// a restarted passthrough channel must still hand players a working key from
// disk, so the still-encrypted segments remain decryptable.
func TestPassthroughKeySurvivesUpstreamDownRestart(t *testing.T) {
	dataDir := t.TempDir()
	origin := startEncryptedOrigin(t, 3, true, nil)

	// --- first boot: ingest while the origin (and key server) is alive ---
	h1 := boot(t, dataDir)
	if err := h1.store.Put("enc", encChannelConfig(origin.URL)); err != nil {
		t.Fatalf("store put: %v", err)
	}
	h1.start(t)

	// Wait for segments, then give the async key-warm a moment to persist.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		w := executeRequest(h1.srv.handleMediaPlaylist, http.MethodGet,
			"/v1/channels/enc/media/v0.m3u8",
			map[string]string{"channelID": "enc", "repID": "v0"})
		if strings.Count(w.Body.String(), "#EXTINF:") >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The key file must exist before we pull the plug.
	keyID := hlskey.KeyID(origin.URL + "/key.bin")
	keyFileDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(keyFileDeadline) {
		kw := executeRequest(h1.srv.handleKeyProxy, http.MethodGet,
			"/v1/channels/enc/key/"+keyID, map[string]string{"channelID": "enc", "keyID": keyID})
		if kw.Code == http.StatusOK {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	h1.stop("enc")

	// --- the origin and its key server vanish entirely ---
	origin.Close()

	// --- restart: everything must come from disk ---
	h2 := boot(t, dataDir)
	h2.start(t)
	t.Cleanup(func() { h2.stop("enc") })

	// The playlist still advertises the key (from the sidecar), and the proxy
	// must now serve the key from the persisted store — no upstream needed.
	kw := executeRequest(h2.srv.handleKeyProxy, http.MethodGet,
		"/v1/channels/enc/key/"+keyID, map[string]string{"channelID": "enc", "keyID": keyID})
	if kw.Code != http.StatusOK {
		t.Fatalf("key proxy after upstream-down restart: got %d, want 200", kw.Code)
	}
	if !bytesEqual(kw.Body.Bytes(), aesTestKey) {
		t.Errorf("restored key = %x, want %x", kw.Body.Bytes(), aesTestKey)
	}

	// End to end: fetch a still-encrypted segment and decrypt it with the
	// disk-served key, proving playback survives with the origin gone.
	segFile := "0.ts"
	sw := executeRequest(h2.srv.handleSegment, http.MethodGet,
		"/v1/channels/enc/segments/v0/"+segFile,
		map[string]string{"channelID": "enc", "repID": "v0", "segFile": segFile})
	if sw.Code != http.StatusOK {
		t.Fatalf("segment after restart: got %d", sw.Code)
	}
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	plain, err := hlskey.Decrypt(sw.Body.Bytes(), kw.Body.Bytes(), iv)
	if err != nil {
		t.Fatalf("decrypt with disk-served key: %v", err)
	}
	if !bytesEqual(plain, tsSegment(0)) {
		t.Error("segment did not decrypt to the original after upstream-down restart")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
