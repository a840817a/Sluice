package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// startFlakyHLSOrigin serves a live 4-segment TS playlist where segment
// flakySeg fails with 503 for the first failCount requests, then succeeds. This
// lets a segment become a genuine hole (retries exhausted) so the poll-driven
// backfill path — not the per-fetch retry — is what must recover it.
func startFlakyHLSOrigin(t *testing.T, flakySeg, failCount int32) *httptest.Server {
	t.Helper()
	var seen int32
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nmedia/v0.m3u8\n")
	})
	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < 4; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.000,\nseg%d.ts\n", i)
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/seg%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		if int32(n) == flakySeg && atomic.AddInt32(&seen, 1) <= failCount {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(tsSegment(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestHLSBackfillsInteriorGap proves the gateway re-fetches a segment that was
// lost to a transient failure (a hole below the high-water mark still in the
// upstream window), rather than leaving the playlist permanently short.
func TestHLSBackfillsInteriorGap(t *testing.T) {
	// Segment 2 fails enough times to exhaust its retries and become a hole,
	// then recovers so a later poll can backfill it.
	origin := startFlakyHLSOrigin(t, 2, 2)

	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 1},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(t.Context(), "bf", ch.Config{
		SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("bf") })
	srv := NewServer(cfg, mgr)

	// The playlist must eventually hold all four contiguous segments. Before the
	// backfill, the hole at 2 collapses the contiguous run — so reaching 4 is
	// proof the gap was filled.
	deadline := time.Now().Add(20 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
			"/v1/channels/bf/media/v0.m3u8",
			map[string]string{"channelID": "bf", "repID": "v0"})
		body = w.Body.String()
		if strings.Count(body, "#EXTINF:") == 4 &&
			strings.Contains(body, "/segments/v0/2.ts") {
			return // backfill succeeded: hole filled, playlist un-collapsed
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("segment 2 was never backfilled; final playlist:\n%s", body)
}
