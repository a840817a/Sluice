package channel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
)

// A restart-safety step that fails while finishing a VOD is reported as a
// warning; it does not stop the channel completing. Only a failed rebuild
// does, and its warnings stay visible in the failed status.

// encryptedHLSOrigin serves one AES-128 segment behind a key endpoint that
// the caller controls. The playlist ends when finished is true.
func encryptedHLSOrigin(t *testing.T, finished bool, key http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nmedia.m3u8\n")
	})
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:1,\nseg.ts\n")
		if finished {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/seg.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 576)) })
	mux.HandleFunc("/key", key)
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	return origin
}

func warningsManager(t *testing.T) (*Manager, *Store, gocfg.Config) {
	t.Helper()
	cfg := gocfg.Config{Store: gocfg.StoreConfig{DataDir: t.TempDir()}, Worker: gocfg.WorkerConfig{FetchWorkers: 1}, Window: gocfg.WindowConfig{Depth: time.Hour}}
	m := newTestManager(t, cfg)
	t.Cleanup(m.StopAll)
	st, err := NewStore(filepath.Join(cfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.store = st
	return m, st, cfg
}

func requireVODWithWarning(t *testing.T, m *Manager, rt *Runtime, done <-chan struct{}, want string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("VOD did not complete (mode %v)", rt.Mode())
	}
	if rt.Mode() != ModeVOD {
		t.Fatalf("mode = %v, want VOD", rt.Mode())
	}
	status, _ := runningStatus(m, rt.ID)
	for _, w := range status.VODWarnings {
		if strings.Contains(w, want) {
			return
		}
	}
	t.Fatalf("vod_warnings = %q, want one containing %q", status.VODWarnings, want)
}

func TestVODCompletesWhenPassthroughKeyIsUnavailable(t *testing.T) {
	origin := encryptedHLSOrigin(t, true, http.NotFound)
	m, st, _ := warningsManager(t)
	cfg := Config{SourceType: SourceHLS, HLSKeyMode: KeyModePassthrough, MPDURL: origin.URL + "/master.m3u8", Enabled: true}
	if err := st.Put("keys", cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.SetOnVODReady(func(*Runtime) { close(done) })
	rt, err := m.startForTest(context.Background(), "keys", cfg)
	if err != nil {
		t.Fatal(err)
	}
	requireVODWithWarning(t, m, rt, done, "checkpoint HLS keys")
	if saved, _ := st.Get("keys"); !saved.IsVOD {
		t.Fatal("VOD state was not persisted")
	}
}

func TestVODCompletesWithStrayTSSegment(t *testing.T) {
	// An empty, never-ending source: nothing is ingested, so the only segment
	// on disk is the stray one, left behind with no sidecar record.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmedia.m3u8\n")
		case "/media.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	m, st, gw := warningsManager(t)
	stray := store.RepLocation{DataDir: gw.Store.DataDir, ChannelID: "stray", PeriodID: "p0", MediaType: index.MediaVideo, ASID: "0", RepID: "v0"}.SegmentPath(7, store.ExtTS)
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{SourceType: SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true}
	if err := st.Put("stray", cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.SetOnVODReady(func(*Runtime) { close(done) })
	rt, err := m.startForTest(context.Background(), "stray", cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for rt.hls == nil || !rt.hls.Describe().Ready {
		if time.Now().After(deadline) {
			t.Fatal("HLS source never completed discovery")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.TransitionToVOD("stray"); err != nil {
		t.Fatal(err)
	}
	requireVODWithWarning(t, m, rt, done, "no restart metadata")
}

// A hot edit must not wait for a key fetch made while finishing a VOD: the
// fetch goes to the network and can take as long as the key server does.
func TestHotEditDoesNotWaitForVODKeyCheckpoint(t *testing.T) {
	var hits atomic.Int32
	checkpointFetch, release := make(chan struct{}), make(chan struct{})
	var fetchOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	origin := encryptedHLSOrigin(t, true, func(w http.ResponseWriter, r *http.Request) {
		// The ingest-time warm fails fast, so the checkpoint has to refetch.
		if hits.Add(1) == 1 {
			http.NotFound(w, r)
			return
		}
		fetchOnce.Do(func() { close(checkpointFetch) })
		select {
		case <-release:
			w.Write([]byte("0123456789abcdef"))
		case <-r.Context().Done():
		}
	})
	m, st, _ := warningsManager(t)
	cfg := Config{SourceType: SourceHLS, HLSKeyMode: KeyModePassthrough, MPDURL: origin.URL + "/master.m3u8", Enabled: true}
	if err := st.Put("edit", cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.SetOnVODReady(func(*Runtime) { close(done) })
	rt, err := m.startForTest(context.Background(), "edit", cfg)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-checkpointFetch:
	case <-time.After(10 * time.Second):
		t.Fatal("VOD transition never refetched the key")
	}

	edited := cfg
	edited.Title = "renamed"
	start := time.Now()
	if _, err := m.UpdateConfig(context.Background(), "edit", edited); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("hot edit waited %v for the key checkpoint", elapsed)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("VOD did not complete after the key was released")
	}
	if got := rt.Config().Title; got != "renamed" {
		t.Fatalf("title after VOD = %q, want the concurrent edit kept", got)
	}
}

func TestVODRebuildFailureKeepsCheckpointWarnings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, rt, blocked := unreadablePeriodsRuntime(t)
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	rt.wg.Wait()
	if rt.Mode() != ModeVODFailed {
		t.Fatalf("mode = %v, want VODFailed", rt.Mode())
	}
	status, _ := runningStatus(m, rt.ID)
	if status.Mode != "vod_failed" || len(status.VODWarnings) == 0 {
		t.Fatalf("status = %q with warnings %q, want vod_failed with the checkpoint's warning", status.Mode, status.VODWarnings)
	}

	// A successful retry replaces the failed attempt's warnings rather than
	// adding to them.
	if err := os.Chmod(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	rt.wg.Wait()
	status, _ = runningStatus(m, rt.ID)
	if status.Mode != "vod" || len(status.VODWarnings) != 0 {
		t.Fatalf("retry status = %q with warnings %q, want vod with none", status.Mode, status.VODWarnings)
	}
}

// unreadablePeriodsRuntime registers a live runtime whose media directory
// cannot be listed: the media checkpoint warns and the rebuild then fails.
func unreadablePeriodsRuntime(t *testing.T) (*Manager, *Runtime, string) {
	t.Helper()
	m := managerForTransition(t)
	t.Cleanup(m.StopAll)
	st, err := NewStore(filepath.Join(m.gwCfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.store = st
	if err := st.Put("unreadable", deadChannel()); err != nil {
		t.Fatal(err)
	}
	rt := registerRuntime(t, m, "unreadable", ModeLive)
	blocked := filepath.Join(m.gwCfg.Store.DataDir, rt.ID, "periods")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	return m, rt, blocked
}

// The key checkpoint has one overall deadline: a key server that accepts the
// request and never answers delays completion by that long, not indefinitely.
func TestVODKeyCheckpointIsBounded(t *testing.T) {
	saved := keyCheckpointTimeout
	keyCheckpointTimeout = 200 * time.Millisecond
	t.Cleanup(func() { keyCheckpointTimeout = saved })

	var hits atomic.Int32
	origin := encryptedHLSOrigin(t, true, func(w http.ResponseWriter, r *http.Request) {
		// The ingest-time warm fails fast, so only the checkpoint's fetch hangs.
		if hits.Add(1) == 1 {
			http.NotFound(w, r)
			return
		}
		<-r.Context().Done()
	})
	m, st, _ := warningsManager(t)
	cfg := Config{SourceType: SourceHLS, HLSKeyMode: KeyModePassthrough, MPDURL: origin.URL + "/master.m3u8", Enabled: true}
	if err := st.Put("slow", cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.SetOnVODReady(func(*Runtime) { close(done) })
	rt, err := m.startForTest(context.Background(), "slow", cfg)
	if err != nil {
		t.Fatal(err)
	}
	requireVODWithWarning(t, m, rt, done, "checkpoint HLS keys")
}
