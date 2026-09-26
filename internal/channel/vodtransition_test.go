package channel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/pipeline"
)

// startEndedHLSOrigin serves a finished HLS asset: one variant, segCount
// segments, EXT-X-ENDLIST. It is the HLS way of saying "the stream is over",
// which is what makes the channel's source return cleanly.
func startEndedHLSOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.4d401e\"\n"+
			"media/v0.m3u8\n")
	})

	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n" +
			"#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n")
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
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(tsSegmentBytes(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// managerWithStore builds a manager backed by a real channels.json, which the
// VOD transition needs: persisting is part of what transitioning means.
func managerWithStore(t *testing.T) (*Manager, *Store, string) {
	t.Helper()
	gwCfg := configForTest(t)
	store, err := NewStore(filepath.Join(gwCfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	m := NewManager(gwCfg, store)
	t.Cleanup(m.StopAll)
	return m, store, gwCfg.Store.DataDir
}

// waitForMode blocks until the channel reaches mode, or fails the test.
func waitForMode(t *testing.T, rt *Runtime, want ChannelMode, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if rt.Mode() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("channel mode is %v after %v, want %v", rt.Mode(), within, want)
}

// TestKeepAllSegmentsDefaultsToKeeping pins the default a config that says
// nothing gets. Everything downstream — ring capacity, expiry, how much history
// survives into VOD — follows from this one answer, and before the flip an
// absent value meant the 120-segment sliding window.
func TestKeepAllSegmentsDefaultsToKeeping(t *testing.T) {
	if !(Config{}).KeepsAllSegments() {
		t.Error("a config with no keep_all_segments does not keep all segments")
	}
	if got := pipeline.SegmentCapacity((Config{}).KeepsAllSegments()); got != 0 {
		t.Errorf("SegmentCapacity for the default config = %d, want 0 (unbounded)", got)
	}
	if (Config{KeepAllSegments: BoolPtr(false)}).KeepsAllSegments() {
		t.Error("an explicit keep_all_segments:false was not honoured")
	}
	if got := pipeline.SegmentCapacity(false); got == 0 {
		t.Error("SegmentCapacity for window mode is unbounded; the window would never expire")
	}
}

// TestUpstreamEndTransitionsToVOD covers the whole point of the change: a
// channel whose config says nothing about retention or transitions still ends
// up as a persisted VOD when the upstream finishes.
func TestUpstreamEndTransitionsToVOD(t *testing.T) {
	origin := startEndedHLSOrigin(t, 3)
	m, store, _ := managerWithStore(t)

	cfg := Config{MPDURL: origin.URL + "/master.m3u8", SourceType: SourceHLS, Enabled: true}
	if err := store.Put("ch", cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	rt, err := m.startForTest(context.Background(), "ch", cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitForMode(t, rt, ModeVOD, 15*time.Second)

	saved, ok := store.Get("ch")
	if !ok {
		t.Fatal("channel disappeared from the store")
	}
	if !saved.IsVOD {
		t.Error("is_vod was not persisted; the channel would come back as live and re-ingest")
	}
}

// TestWindowModeChannelStillTransitions pins the decision that the transition
// is unconditional. Retention decides how much history the VOD covers, never
// whether the channel is allowed to finish — a window-mode channel whose
// upstream ended must not keep claiming to be live.
func TestWindowModeChannelStillTransitions(t *testing.T) {
	origin := startEndedHLSOrigin(t, 3)
	m, store, _ := managerWithStore(t)

	cfg := Config{
		MPDURL:          origin.URL + "/master.m3u8",
		SourceType:      SourceHLS,
		Enabled:         true,
		KeepAllSegments: BoolPtr(false),
	}
	if err := store.Put("ch", cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	rt, err := m.startForTest(context.Background(), "ch", cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitForMode(t, rt, ModeVOD, 15*time.Second)

	if saved, _ := store.Get("ch"); !saved.IsVOD {
		t.Error("a window-mode channel reached VOD without persisting is_vod")
	}
}

// TestRestoreToLiveSuppressesImmediateRetransition is the reverse operation,
// and the suppression that makes it worth having.
//
// The upstream in this test is still finished, which is the realistic case: an
// operator restores a channel that transitioned on an upstream hiccup. Without
// the suppression the source completes cleanly again and the 2-second drain
// ticker puts the channel straight back into VOD, so the button would appear to
// do nothing. The late re-check is what fails in that case.
func TestRestoreToLiveSuppressesImmediateRetransition(t *testing.T) {
	origin := startEndedHLSOrigin(t, 3)
	m, store, dataDir := managerWithStore(t)

	cfg := Config{MPDURL: origin.URL + "/master.m3u8", SourceType: SourceHLS, Enabled: true}
	if err := store.Put("ch", cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	rt, err := m.startForTest(context.Background(), "ch", cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForMode(t, rt, ModeVOD, 15*time.Second)
	// Optional local exports follow VOD publication. Join the completed
	// VOD transition before taking a baseline of every file, including exports.
	rt.wg.Wait()
	before := filesUnder(t, filepath.Join(dataDir, "ch"))

	if err := m.RestoreToLive(context.Background(), "ch"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	saved, ok := store.Get("ch")
	if !ok {
		t.Fatal("channel disappeared from the store")
	}
	if saved.IsVOD {
		t.Error("is_vod is still set after a restore; a gateway restart would come back as VOD")
	}

	rt = m.Get("ch")
	if rt == nil {
		t.Fatal("channel is not running after a restore")
	}
	if got := rt.Mode(); got != ModeLive {
		t.Fatalf("mode is %v right after a restore, want live", got)
	}

	// Long enough to cover a re-run of the source plus the drain ticker that
	// would follow it.
	time.Sleep(6 * time.Second)
	if got := rt.Mode(); got != ModeLive {
		t.Errorf("mode is %v six seconds after a restore, want live: "+
			"the finished upstream transitioned the channel straight back to VOD", got)
	}

	if after := filesUnder(t, filepath.Join(dataDir, "ch")); len(after) != len(before) {
		t.Errorf("segment files went from %d to %d across a restore; nothing on disk may be touched",
			len(before), len(after))
	}
}

// TestRestoreToLiveRejectsALiveChannel pins the precondition, since the handler
// turns this error into a 400.
func TestRestoreToLiveRejectsALiveChannel(t *testing.T) {
	m, store, _ := managerWithStore(t)
	cfg := deadChannel()
	if err := store.Put("ch", cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := m.startForTest(context.Background(), "ch", cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.RestoreToLive(context.Background(), "ch"); err == nil {
		t.Error("restoring a live channel succeeded; it would pointlessly restart it")
	}
	if err := m.RestoreToLive(context.Background(), "nosuch"); err == nil {
		t.Error("restoring an unknown channel succeeded")
	}
}
