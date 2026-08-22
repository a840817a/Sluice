package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
)

// TestHLSLiveRestartResumesWithoutCollapse is a regression test.
//
// An HLS live channel that restarts must seed its per-variant polling state
// from the index (like the DASH watcher seeds lastKnown), otherwise the first
// re-poll re-fetches the whole current window, re-commits segments already on
// disk as duplicate index entries, and the playlist — which requires a strictly
// contiguous run — collapses to a single segment.
func TestHLSLiveRestartResumesWithoutCollapse(t *testing.T) {
	dataDir := t.TempDir()
	origin := startHLSOrigin(t, 4) // static 4-segment live playlist (no ENDLIST)

	h1 := boot(t, dataDir)
	if err := h1.store.Put("live1", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	h1.start(t)
	h1.waitForSegments(t, "live1", 4)
	h1.stop("live1")

	h2 := boot(t, dataDir)
	h2.start(t)
	t.Cleanup(func() { h2.stop("live1") })

	// Let the watcher complete its first re-poll (and any erroneous re-fetch).
	time.Sleep(2 * time.Second)

	_, body := h2.playlist(t, "live1")

	if n := strings.Count(body, "#EXTINF:"); n != 4 {
		t.Fatalf("after restart the live playlist has %d segments, want 4 — it either collapsed or duplicated:\n%s", n, body)
	}
	// No segment may appear twice.
	counts := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "/segments/live1/v0/") {
			counts[line]++
		}
	}
	for uri, n := range counts {
		if n > 1 {
			t.Errorf("segment appears %d times (duplicate) after restart: %s", n, uri)
		}
	}

	// The channel must keep running (a live channel, not terminated).
	w := executeRequest(h2.srv.handleMediaPlaylist, http.MethodGet,
		"/v1/channels/live1/media/v0.m3u8",
		map[string]string{"channelID": "live1", "repID": "v0"})
	if strings.Contains(w.Body.String(), "#EXT-X-ENDLIST") {
		t.Error("live channel came back terminated after restart")
	}
}
