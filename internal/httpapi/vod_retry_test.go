package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
)

// startEmptyLiveHLSOrigin provides a source that has valid, checkpointable HLS
// discovery state but never completes and never admits segment work.
func startEmptyLiveHLSOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmedia.m3u8\n"))
		case "/media.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	return origin
}

func waitForHLSDiscovery(t *testing.T, rt *ch.Runtime) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !rt.Serving().Tracks.Ready {
		if time.Now().After(deadline) {
			t.Fatal("HLS source never completed discovery")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAdminVODRetryAfterRebuildFailure(t *testing.T) {
	admin, dir := newTestAdminServerIn(t)
	t.Cleanup(admin.manager.StopAll)
	// A reachable, empty live HLS source lets the manual transition checkpoint
	// valid source state without admitting any segment work. The `periods`
	// blocker below is therefore the only reason the first attempt fails.
	origin := startEmptyLiveHLSOrigin(t)
	blocker := filepath.Join(dir, "retry", "periods")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := adminExec(admin, http.MethodPost, "/admin/api/channels", map[string]any{
		"id": "retry", "mpd_url": origin.URL + "/master.m3u8", "source_type": "hls",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	rt := admin.manager.Get("retry")
	waitForHLSDiscovery(t, rt)
	transition := func() {
		t.Helper()
		w := adminCall(admin.handleTransitionToVOD, http.MethodPost,
			"/admin/api/channels/retry/transition-to-vod", map[string]string{"channelID": "retry"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("transition: %d %s", w.Code, w.Body.String())
		}
	}
	waitMode := func(want ch.ChannelMode) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for rt.Mode() != want {
			if time.Now().After(deadline) {
				t.Fatalf("mode = %v, want %v", rt.Mode(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	transition()
	waitMode(ch.ModeVODFailed)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	transition() // Previously returned HTTP 400 for the stuck transitioning mode.
	waitMode(ch.ModeVOD)
	if admin.manager.Get("retry") != rt {
		t.Fatal("retry replaced the runtime instead of rebuilding the stopped ingest")
	}
}
