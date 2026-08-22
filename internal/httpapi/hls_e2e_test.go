package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// tsSegment builds a deterministic but distinct MPEG-TS payload for segment n,
// so the served bytes can be compared against the origin's byte for byte.
func tsSegment(n int) []byte {
	const packets = 3
	out := make([]byte, 0, packets*188)
	for p := 0; p < packets; p++ {
		pkt := make([]byte, 188)
		pkt[0] = 0x47 // sync byte
		pkt[1] = byte(n)
		pkt[2] = byte(p)
		for i := 3; i < 188; i++ {
			pkt[i] = byte((n*31 + p*7 + i) % 251)
		}
		out = append(out, pkt...)
	}
	return out
}

// startHLSOrigin serves a master playlist, one media playlist with segCount
// MPEG-TS segments, and the segments themselves.
func startHLSOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-VERSION:3\n"+
			"#EXT-X-INDEPENDENT-SEGMENTS\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\"\n"+
			"media/v0.m3u8\n")
	})

	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < segCount; i++ {
			// Relative URI resolved against the media playlist's own directory.
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
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(tsSegment(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// waitForPlaylist polls the media playlist handler until it advertises want
// segments, or fails the test.
func waitForPlaylist(t *testing.T, srv *Server, channelID, repID string, want int) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
			"/v1/channels/"+channelID+"/media/"+repID+".m3u8",
			map[string]string{"channelID": channelID, "repID": repID})
		body = w.Body.String()
		if w.Code == http.StatusOK && strings.Count(body, "#EXTINF:") >= want {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d segments; last playlist (code shown above):\n%s", want, body)
	return ""
}

// startHLSChannel boots a gateway against an HLS origin and returns the server.
func startHLSChannel(t *testing.T, originURL string) (*Server, config.Config) {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "hls1", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     originURL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start HLS channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("hls1") })
	return NewServer(cfg, mgr), cfg
}

// TestHLSEndToEndTSPassthrough drives the whole Phase 1 path: an HLS origin
// with MPEG-TS segments is discovered, ingested, and re-served as HLS, with the
// segment bytes untouched.
func TestHLSEndToEndTSPassthrough(t *testing.T) {
	origin := startHLSOrigin(t, 3)
	srv, _ := startHLSChannel(t, origin.URL)

	body := waitForPlaylist(t, srv, "hls1", "v0", 3)

	// --- media playlist ---
	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:4",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXTINF:4.000000,",
		"http://gw.test/v1/channels/hls1/segments/v0/0.ts",
		"http://gw.test/v1/channels/hls1/segments/v0/2.ts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("media playlist missing %q:\n%s", want, body)
		}
	}
	// A TS playlist must not advertise an initialization section.
	if strings.Contains(body, "EXT-X-MAP") {
		t.Errorf("TS playlist must not contain EXT-X-MAP:\n%s", body)
	}
	// Live playlists stay open.
	if strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Errorf("live playlist must not be terminated:\n%s", body)
	}

	// --- master playlist ---
	w := executeRequest(srv.handleMasterPlaylist, http.MethodGet,
		"/v1/channels/hls1/master.m3u8", map[string]string{"channelID": "hls1"})
	if w.Code != http.StatusOK {
		t.Fatalf("master playlist: got %d: %s", w.Code, w.Body.String())
	}
	master := w.Body.String()
	for _, want := range []string{
		"#EXT-X-INDEPENDENT-SEGMENTS",
		"BANDWIDTH=800000",
		"RESOLUTION=640x360",
		`CODECS="avc1.4d401e,mp4a.40.2"`,
		"http://gw.test/v1/channels/hls1/media/v0.m3u8",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master playlist missing %q:\n%s", want, master)
		}
	}

	// --- byte-for-byte segment passthrough ---
	for n := 0; n < 3; n++ {
		segFile := fmt.Sprintf("%d.ts", n)
		w := executeRequest(srv.handleSegment, http.MethodGet,
			"/v1/channels/hls1/segments/v0/"+segFile,
			map[string]string{"channelID": "hls1", "repID": "v0", "segFile": segFile})
		if w.Code != http.StatusOK {
			t.Fatalf("segment %s: got %d", segFile, w.Code)
		}
		if got, want := w.Body.Bytes(), tsSegment(n); string(got) != string(want) {
			t.Errorf("segment %s was not passed through unchanged (%d bytes vs %d)",
				segFile, len(got), len(want))
		}
		if ct := w.Header().Get("Content-Type"); ct != "video/mp2t" {
			t.Errorf("segment %s Content-Type = %q, want video/mp2t", segFile, ct)
		}
	}
}

// TestHLSChannelRejectsDASHOutput enforces the rule that an MPEG-TS source is
// never served as DASH, since that would require remuxing.
func TestHLSChannelRejectsDASHOutput(t *testing.T) {
	origin := startHLSOrigin(t, 2)
	srv, _ := startHLSChannel(t, origin.URL)
	waitForPlaylist(t, srv, "hls1", "v0", 2)

	w := executeRequest(srv.handleManifest, http.MethodGet,
		"/v1/channels/hls1/manifest.mpd", map[string]string{"channelID": "hls1"})
	if w.Code != http.StatusNotFound {
		t.Errorf("manifest.mpd for a TS HLS channel: got %d, want 404", w.Code)
	}
}

// TestHLSUnknownRepIs404 keeps a bad representation a hard 404 rather than an
// empty playlist that a player would treat as a valid but stalled stream.
func TestHLSUnknownRepIs404(t *testing.T) {
	origin := startHLSOrigin(t, 2)
	srv, _ := startHLSChannel(t, origin.URL)
	waitForPlaylist(t, srv, "hls1", "v0", 2)

	w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
		"/v1/channels/hls1/media/nope.m3u8",
		map[string]string{"channelID": "hls1", "repID": "nope"})
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown rep: got %d, want 404", w.Code)
	}
}

// startHLSVODOrigin serves a finished playlist: one terminated by EXT-X-ENDLIST.
func startHLSVODOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		// RESOLUTION and CODECS are carried so tests can assert the per-track
		// breakdown the admin API decorates from this playlist.
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\"\n"+
			"media/v0.m3u8\n")
	})
	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < segCount; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.000,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/seg%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(tsSegment(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestHLSVODKeepsItsSegments is a regression test.
//
// An HLS source ending in EXT-X-ENDLIST makes the channel transition to VOD.
// That transition used to rebuild the index from disk, which only works for
// fMP4 — TS segments carry no tfdt, so the rebuild produced an empty index and
// the channel served a playlist with zero segments despite every .ts file
// being present on disk.
func TestHLSVODKeepsItsSegments(t *testing.T) {
	origin := startHLSVODOrigin(t, 4)
	srv, _ := startHLSChannel(t, origin.URL)

	// Wait for the channel to settle into VOD (the transition is asynchronous).
	deadline := time.Now().Add(20 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
			"/v1/channels/hls1/media/v0.m3u8",
			map[string]string{"channelID": "hls1", "repID": "v0"})
		body = w.Body.String()
		if strings.Contains(body, "#EXT-X-ENDLIST") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Fatalf("channel never reached VOD:\n%s", body)
	}
	if n := strings.Count(body, "#EXTINF:"); n != 4 {
		t.Fatalf("VOD playlist has %d segments, want 4 — the index was lost in the VOD transition:\n%s", n, body)
	}
	if strings.Contains(body, "#EXT-X-TARGETDURATION:0") {
		t.Errorf("VOD playlist advertises a zero target duration:\n%s", body)
	}
	// Segments must still be served after the transition.
	w := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/hls1/segments/v0/0.ts",
		map[string]string{"channelID": "hls1", "repID": "v0", "segFile": "0.ts"})
	if w.Code != http.StatusOK {
		t.Errorf("segment after VOD transition: got %d, want 200", w.Code)
	}
	if got := w.Body.Bytes(); string(got) != string(tsSegment(0)) {
		t.Error("segment corrupted across the VOD transition")
	}
}

// TestHealthAdvertisesPlayableURL checks that clients can discover which URL a
// channel serves without having to guess its protocol.
func TestHealthAdvertisesPlayableURL(t *testing.T) {
	origin := startHLSOrigin(t, 2)
	srv, _ := startHLSChannel(t, origin.URL)
	waitForPlaylist(t, srv, "hls1", "v0", 2)

	w := executeRequest(srv.handleChannelHealth, http.MethodGet,
		"/v1/channels/hls1/health", map[string]string{"channelID": "hls1"})
	if w.Code != http.StatusOK {
		t.Fatalf("health: got %d", w.Code)
	}
	var resp struct {
		SourceType  string `json:"source_type"`
		ManifestURL string `json:"manifest_url"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if resp.SourceType != "hls" {
		t.Errorf("source_type = %q, want hls", resp.SourceType)
	}
	if resp.ManifestURL != "/v1/channels/hls1/master.m3u8" {
		t.Errorf("manifest_url = %q, want the HLS master playlist", resp.ManifestURL)
	}
}

// TestDASHChannelHLSNotReadyBeforeMPD checks that a DASH channel whose upstream
// MPD has not been fetched yet reports its HLS output as not-ready (503) rather
// than unavailable — clear DASH sources are serveable as HLS once discovered.
func TestDASHChannelHLSNotReadyBeforeMPD(t *testing.T) {
	cfg := config.Config{Store: config.StoreConfig{DataDir: t.TempDir()}}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "dash1", ch.Config{
		MPDURL:  "http://127.0.0.1:0/live.mpd", // unreachable: never becomes ready
		Enabled: true,
	}); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("dash1") })
	srv := NewServer(cfg, mgr)

	w := executeRequest(srv.handleMasterPlaylist, http.MethodGet,
		"/v1/channels/dash1/master.m3u8", map[string]string{"channelID": "dash1"})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("master.m3u8 for a not-yet-discovered DASH channel: got %d, want 503", w.Code)
	}
}
