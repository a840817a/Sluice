package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// These tests drive the VOD origin from hls_e2e_test.go: a playlist terminated by
// EXT-X-ENDLIST on its first response, which is what makes its length trustworthy
// as a segment total.

// adminList calls the channel-list handler directly. The shared adminExec helper
// dispatches on HTTP method alone, so every GET reaches handleChannelStatus and
// the list endpoint cannot be exercised through it.
func adminList(admin *AdminServer) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/channels", nil)
	w := httptest.NewRecorder()
	admin.handleListChannels(w, req)
	return w
}

// progressOf pulls one channel's progress object out of the real admin list
// endpoint, so the test reads exactly what the UI reads.
func progressOf(t *testing.T, admin *AdminServer, channelID string) *ch.ProgressStats {
	t.Helper()
	w := adminList(admin)
	if w.Code != http.StatusOK {
		t.Fatalf("list channels: status %d, body %s", w.Code, w.Body.String())
	}
	var out map[string]struct {
		Status ch.ChannelStatus `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v\nbody: %s", err, w.Body.String())
	}
	entry, ok := out[channelID]
	if !ok {
		t.Fatalf("channel %q absent from the list response: %s", channelID, w.Body.String())
	}
	return entry.Status.Progress
}

// startVODChannelWithAdmin boots a gateway against a VOD HLS origin and returns
// an AdminServer sharing the same Manager.
func startVODChannelWithAdmin(t *testing.T, originURL, channelID string) *AdminServer {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dir},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	store, err := ch.NewStore(filepath.Join(dir, "channels.json"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	chCfg := ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     originURL + "/master.m3u8",
		Enabled:    true,
	}
	if err := store.Put(channelID, chCfg); err != nil {
		t.Fatalf("store put: %v", err)
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), channelID, chCfg); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop(channelID) })
	return NewAdminServer(context.Background(), mgr, store)
}

// TestAdminProgressReportsVODDownload is the end-to-end proof of the download
// progress display: a bounded source must report a real total and a stored count
// that converges on it.
//
// The bug this replaces made the old UI show 100% permanently, because the total
// it divided by was derived from the same ring buffer as the completed count.
func TestAdminProgressReportsVODDownload(t *testing.T) {
	const segCount = 6
	origin := startHLSVODOrigin(t, segCount)
	admin := startVODChannelWithAdmin(t, origin.URL, "vod1")

	// Wait for ingest to finish, reading only through the admin API.
	var final *ch.ProgressStats
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p := progressOf(t, admin, "vod1")
		if p != nil && p.SegmentsStored >= segCount && p.TotalKnown {
			final = p
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if final == nil {
		p := progressOf(t, admin, "vod1")
		t.Fatalf("timed out waiting for %d segments; last progress: %+v", segCount, p)
	}

	if final.TotalSegments != segCount {
		t.Errorf("TotalSegments = %d, want %d", final.TotalSegments, segCount)
	}
	if !final.TotalKnown {
		t.Error("TotalKnown = false for a VOD source, want true")
	}
	if final.TotalCapped {
		t.Error("TotalCapped = true for a 6-segment playlist, want false")
	}
	if final.SegmentsStored != segCount {
		t.Errorf("SegmentsStored = %d, want %d", final.SegmentsStored, segCount)
	}
	// The count must never exceed the total. A duplicate commit counted twice is
	// exactly how a download bar ends up reporting 110%.
	if final.SegmentsStored > final.TotalSegments {
		t.Errorf("SegmentsStored %d exceeds TotalSegments %d",
			final.SegmentsStored, final.TotalSegments)
	}
	if final.BytesStored == 0 {
		t.Error("BytesStored = 0 after storing segments")
	}
	if final.SegmentsDropped != 0 {
		t.Errorf("SegmentsDropped = %d on a healthy ingest, want 0", final.SegmentsDropped)
	}
	if final.SourceError != nil {
		t.Errorf("SourceError = %+v on a healthy ingest, want nil", final.SourceError)
	}
	if final.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if final.LastSegmentAt == nil {
		t.Error("LastSegmentAt is nil after storing segments")
	}
}

// TestAdminProgressSurvivesRepeatedPolling pins that reading the status twice in
// a row does not change it. The UI polls every two seconds, so any accidental
// mutation in the read path would corrupt the numbers it displays.
func TestAdminProgressSurvivesRepeatedPolling(t *testing.T) {
	const segCount = 4
	origin := startHLSVODOrigin(t, segCount)
	admin := startVODChannelWithAdmin(t, origin.URL, "vod2")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p := progressOf(t, admin, "vod2"); p != nil && p.SegmentsStored >= segCount {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	first := progressOf(t, admin, "vod2")
	second := progressOf(t, admin, "vod2")
	if first == nil || second == nil {
		t.Fatal("progress absent")
	}
	if first.SegmentsStored != second.SegmentsStored {
		t.Errorf("SegmentsStored changed between reads: %d then %d",
			first.SegmentsStored, second.SegmentsStored)
	}
	if first.TotalSegments != second.TotalSegments {
		t.Errorf("TotalSegments changed between reads: %d then %d",
			first.TotalSegments, second.TotalSegments)
	}
}

// TestAdminStatusIncludesTracks checks the per-track breakdown reaches the API
// with its descriptive fields populated from the HLS multivariant playlist.
func TestAdminStatusIncludesTracks(t *testing.T) {
	origin := startHLSVODOrigin(t, 3)
	admin := startVODChannelWithAdmin(t, origin.URL, "vod3")

	var tracks []ch.TrackStats
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		w := adminExec(admin, http.MethodGet, "/admin/api/channels/vod3/status", nil,
			map[string]string{"channelID": "vod3"})
		if w.Code == http.StatusOK {
			var st ch.ChannelStatus
			if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if len(st.Tracks) > 0 {
				tracks = st.Tracks
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(tracks) == 0 {
		t.Fatal("timed out waiting for tracks to appear in the status response")
	}

	v0 := tracks[0]
	if v0.RepID != "v0" {
		t.Errorf("tracks[0].RepID = %q, want v0", v0.RepID)
	}
	// Decoration from EXT-X-STREAM-INF: BANDWIDTH=800000, RESOLUTION=640x360.
	if v0.Bandwidth != 800000 {
		t.Errorf("Bandwidth = %d, want 800000", v0.Bandwidth)
	}
	if v0.Width != 640 || v0.Height != 360 {
		t.Errorf("resolution = %dx%d, want 640x360", v0.Width, v0.Height)
	}
	if v0.Codecs == "" {
		t.Error("Codecs is empty, want the value from the multivariant playlist")
	}
	if v0.Published+v0.Committed == 0 {
		t.Errorf("track has no segments: %+v", v0)
	}
}

// TestAdminStatusOmitsProgressForStoppedChannel pins that a configured but
// not-running channel reports no progress object at all, rather than a zeroed one
// that would render as a stalled channel.
func TestAdminStatusOmitsProgressForStoppedChannel(t *testing.T) {
	// The config goes straight into the store rather than through the create
	// endpoint, which always starts what it creates (it forces Enabled = true).
	// A channel present in the store but absent from the Manager is exactly the
	// state this test is about.
	dir := t.TempDir()
	store, err := ch.NewStore(filepath.Join(dir, "channels.json"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := store.Put("stopped1", ch.Config{
		MPDURL:  "http://origin.invalid/live.mpd",
		Enabled: false,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	mgr := ch.NewManager(config.Config{Store: config.StoreConfig{DataDir: dir}})
	admin := NewAdminServer(context.Background(), mgr, store)

	list := adminList(admin)
	var out map[string]struct {
		Status map[string]json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	entry, ok := out["stopped1"]
	if !ok {
		t.Fatalf("channel absent: %s", list.Body.String())
	}
	if _, present := entry.Status["progress"]; present {
		t.Errorf("progress key present for a stopped channel: %s", list.Body.String())
	}
	if _, present := entry.Status["tracks"]; present {
		t.Errorf("tracks key present for a stopped channel: %s", list.Body.String())
	}
}
