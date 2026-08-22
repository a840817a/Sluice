package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// restartHarness keeps the settings that must stay identical across a restart.
type restartHarness struct {
	cfg   config.Config
	store *ch.Store
	mgr   *ch.Manager
	srv   *Server
}

// boot starts a gateway against dataDir, as a process start would.
func boot(t *testing.T, dataDir string) *restartHarness {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dataDir},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	store, err := ch.NewStore(filepath.Join(dataDir, "channels.json"))
	if err != nil {
		t.Fatalf("open channel store: %v", err)
	}
	mgr := ch.NewManager(cfg)
	mgr.SetStore(store)
	return &restartHarness{cfg: cfg, store: store, mgr: mgr, srv: NewServer(cfg, mgr)}
}

// start launches every channel the store knows about, as startup does.
func (h *restartHarness) start(t *testing.T) {
	t.Helper()
	for id, cfg := range h.store.List() {
		if !cfg.Enabled {
			continue
		}
		if _, err := h.mgr.Start(context.Background(), id, cfg); err != nil {
			t.Fatalf("start channel %s: %v", id, err)
		}
	}
}

func (h *restartHarness) stop(id string) { _ = h.mgr.Stop(id) }

func (h *restartHarness) playlist(t *testing.T, id string) (int, string) {
	t.Helper()
	w := executeRequest(h.srv.handleMediaPlaylist, http.MethodGet,
		"/v1/channels/"+id+"/media/v0.m3u8",
		map[string]string{"channelID": id, "repID": "v0"})
	return w.Code, w.Body.String()
}

func (h *restartHarness) waitForSegments(t *testing.T, id string, want int) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		code, b := h.playlist(t, id)
		body = b
		if code == http.StatusOK && strings.Count(body, "#EXTINF:") >= want {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d segments on %s:\n%s", want, id, body)
	return ""
}

// TestHLSVODSurvivesRestartWithOriginOffline is the Phase 3 acceptance test.
//
// A finished HLS stream is ingested, the gateway restarts, and the origin is
// gone. Everything served afterwards must come from disk: the segment files
// plus the per-representation metadata sidecar, since MPEG-TS carries no timing
// of its own.
func TestHLSVODSurvivesRestartWithOriginOffline(t *testing.T) {
	dataDir := t.TempDir()
	origin := startHLSVODOrigin(t, 4)

	// --- first boot: ingest the whole stream ---
	h1 := boot(t, dataDir)
	if err := h1.store.Put("vod1", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	h1.start(t)

	// Wait for the channel to finish and settle into VOD.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, b := h1.playlist(t, "vod1"); strings.Contains(b, "#EXT-X-ENDLIST") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	before := h1.waitForSegments(t, "vod1", 4)
	if !strings.Contains(before, "#EXT-X-ENDLIST") {
		t.Fatalf("channel never reached VOD before restart:\n%s", before)
	}
	// VOD state must have been persisted for the restart to restore it.
	if cfg, ok := h1.store.Get("vod1"); !ok || !cfg.IsVOD {
		t.Fatalf("IsVOD not persisted for the HLS channel: %+v", cfg)
	}
	h1.stop("vod1")

	// --- the origin disappears ---
	origin.Close()

	// --- second boot: everything must come from disk ---
	h2 := boot(t, dataDir)
	h2.start(t)
	t.Cleanup(func() { h2.stop("vod1") })

	code, after := h2.playlist(t, "vod1")
	if code != http.StatusOK {
		t.Fatalf("playlist after restart: got %d:\n%s", code, after)
	}
	if n := strings.Count(after, "#EXTINF:"); n != 4 {
		t.Fatalf("after restart %d segments, want 4 — the DVR window was lost:\n%s", n, after)
	}
	if !strings.Contains(after, "#EXT-X-ENDLIST") {
		t.Errorf("restored VOD playlist is not terminated:\n%s", after)
	}
	// Durations must be restored, not guessed.
	if !strings.Contains(after, "#EXTINF:4.000000,") {
		t.Errorf("segment durations not restored from the sidecar:\n%s", after)
	}
	if strings.Contains(after, "#EXT-X-TARGETDURATION:0") {
		t.Errorf("restored playlist advertises a zero target duration:\n%s", after)
	}

	// Segments must still be servable, byte for byte.
	for n := 0; n < 4; n++ {
		segFile := fmt.Sprintf("%d.ts", n)
		w := executeRequest(h2.srv.handleSegment, http.MethodGet,
			"/v1/channels/vod1/segments/v0/"+segFile,
			map[string]string{"channelID": "vod1", "repID": "v0", "segFile": segFile})
		if w.Code != http.StatusOK {
			t.Fatalf("segment %s after restart: got %d", segFile, w.Code)
		}
		if got := w.Body.Bytes(); string(got) != string(tsSegment(n)) {
			t.Errorf("segment %s corrupted across restart", segFile)
		}
	}
}

// TestHLSLiveDVRSurvivesRestart covers the live case: the channel keeps
// ingesting after a restart, but the history recorded before it is still there.
func TestHLSLiveDVRSurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	origin := startHLSOrigin(t, 4) // live: no EXT-X-ENDLIST

	h1 := boot(t, dataDir)
	if err := h1.store.Put("live1", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	h1.start(t)
	h1.waitForSegments(t, "live1", 4)
	h1.stop("live1")

	h2 := boot(t, dataDir)
	h2.start(t)
	t.Cleanup(func() { h2.stop("live1") })

	// The pre-restart segments must be present immediately, without waiting for
	// them to be re-fetched.
	code, after := h2.playlist(t, "live1")
	if code != http.StatusOK {
		t.Fatalf("playlist after restart: got %d", code)
	}
	if n := strings.Count(after, "#EXTINF:"); n < 4 {
		t.Errorf("after restart %d segments, want at least the 4 recorded before:\n%s", n, after)
	}
	// A live channel must not come back terminated.
	if strings.Contains(after, "#EXT-X-ENDLIST") {
		t.Errorf("live channel restored as ended:\n%s", after)
	}
}
