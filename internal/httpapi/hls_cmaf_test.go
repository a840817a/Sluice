package httpapi

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// mp4box wraps payload in an ISO-BMFF box header (size + fourcc).
func mp4box(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	out = append(out, []byte(typ)...)
	return append(out, body...)
}

// fmp4Init builds a minimal but valid CMAF initialization segment carrying the
// given media timescale — enough for ValidateInit and the mdhd timescale reader.
func fmp4Init(timescale uint32) []byte {
	ts := make([]byte, 4)
	binary.BigEndian.PutUint32(ts, timescale)
	mdhd := []byte{0, 0, 0, 0} // version 0 + flags
	mdhd = append(mdhd, 0, 0, 0, 0, 0, 0, 0, 0)
	mdhd = append(mdhd, ts...)
	mdhd = append(mdhd, 0, 0, 0, 0) // duration
	mdhd = append(mdhd, 0x55, 0xc4) // language "und"
	mdhd = append(mdhd, 0, 0)       // pre_defined
	return mp4box("moov", mp4box("trak", mp4box("mdia", mp4box("mdhd", mdhd))))
}

// fmp4Segment builds a minimal media segment with the given decode time, so it
// passes ValidateSegment (moof + mdat + tfdt) and ReadStartPTS.
func fmp4Segment(baseMediaDecodeTime uint64) []byte {
	bmdt := make([]byte, 8)
	binary.BigEndian.PutUint64(bmdt, baseMediaDecodeTime)
	tfdt := append([]byte{1, 0, 0, 0}, bmdt...) // version 1
	moof := mp4box("moof", mp4box("traf", mp4box("tfdt", tfdt)))
	mdat := mp4box("mdat", []byte{0, 0, 0, 0})
	return append(moof, mdat...)
}

const cmafTimescale = 12800

// startCMAFOrigin serves a fMP4 (CMAF) HLS stream with an EXT-X-MAP init.
func startCMAFOrigin(t *testing.T, segCount int, endList bool) *httptest.Server {
	return startCMAFOriginOpts(t, segCount, endList, -1)
}

// startCMAFOriginOpts additionally emits EXT-X-DISCONTINUITY before the segment
// at discontinuityAt (-1 for none).
func startCMAFOriginOpts(t *testing.T, segCount int, endList bool, discontinuityAt int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.42c01e,mp4a.40.2\"\n"+
			"media/v0.m3u8\n")
	})
	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-MAP:URI="init.mp4"` + "\n")
		for i := 0; i < segCount; i++ {
			if i == discontinuityAt {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			fmt.Fprintf(&b, "#EXTINF:4.000,\nseg%d.m4s\n", i)
		}
		if endList {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/media/init.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fmp4Init(cmafTimescale))
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/seg%d.m4s", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(fmp4Segment(uint64(n) * 4 * cmafTimescale))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func startCMAFChannel(t *testing.T, originURL string) *Server {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "cmaf", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     originURL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start CMAF channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("cmaf") })
	return NewServer(cfg, mgr)
}

func waitForCMAFPlaylist(t *testing.T, srv *Server, want int) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
			"/v1/channels/cmaf/media/v0.m3u8",
			map[string]string{"channelID": "cmaf", "repID": "v0"})
		body = w.Body.String()
		if w.Code == http.StatusOK && strings.Count(body, "#EXTINF:") >= want {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d CMAF segments:\n%s", want, body)
	return ""
}

// TestCMAFDualOutput is the Phase 4 acceptance test: one CMAF channel serves a
// DASH manifest and an HLS playlist from the very same files on disk.
func TestCMAFDualOutput(t *testing.T) {
	origin := startCMAFOrigin(t, 4, true)
	srv := startCMAFChannel(t, origin.URL)

	// The source ends with EXT-X-ENDLIST, so wait for the channel to settle into
	// VOD before asserting a static manifest.
	hls := waitForCMAFPlaylist(t, srv, 4)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(hls, "#EXT-X-ENDLIST") {
		time.Sleep(100 * time.Millisecond)
		hls = waitForCMAFPlaylist(t, srv, 4)
	}
	if !strings.Contains(hls, "#EXT-X-ENDLIST") {
		t.Fatalf("CMAF channel never reached VOD:\n%s", hls)
	}

	// --- HLS output: fMP4 with an EXT-X-MAP, version 6+ ---
	if !strings.Contains(hls, "#EXT-X-MAP:URI=") {
		t.Errorf("fMP4 HLS playlist must advertise an init map:\n%s", hls)
	}
	if !strings.Contains(hls, "/segments/v0/0.m4s") {
		t.Errorf("HLS must reference .m4s segments:\n%s", hls)
	}

	// --- DASH output from the same channel ---
	dw := executeRequest(srv.handleManifest, http.MethodGet,
		"/v1/channels/cmaf/manifest.mpd", map[string]string{"channelID": "cmaf"})
	if dw.Code != http.StatusOK {
		t.Fatalf("manifest.mpd for a CMAF channel: got %d, want 200:\n%s", dw.Code, dw.Body.String())
	}
	mpd := dw.Body.String()
	for _, want := range []string{
		`type="static"`,
		`mimeType="video/mp4"`,
		`codecs="avc1.42c01e,mp4a.40.2"`,
		`<SegmentTimeline>`,
		fmt.Sprintf(`timescale="%d"`, cmafTimescale),
		"v1/channels/cmaf/segments/v0/",
		"v1/channels/cmaf/init/v0.mp4",
	} {
		if !strings.Contains(mpd, want) {
			t.Errorf("DASH manifest missing %q:\n%s", want, mpd)
		}
	}

	// --- both outputs point at the same files on disk ---
	// DASH uses zero-padded names ($Number%09d$), HLS uses bare numbers; both
	// must resolve to the same stored segment.
	padded := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/cmaf/segments/v0/000000000.m4s",
		map[string]string{"channelID": "cmaf", "repID": "v0", "segFile": "000000000.m4s"})
	bare := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/cmaf/segments/v0/0.m4s",
		map[string]string{"channelID": "cmaf", "repID": "v0", "segFile": "0.m4s"})
	if padded.Code != http.StatusOK || bare.Code != http.StatusOK {
		t.Fatalf("segment fetch: padded=%d bare=%d", padded.Code, bare.Code)
	}
	if !strings.EqualFold(padded.Body.String(), bare.Body.String()) {
		t.Error("DASH and HLS segment URLs resolved to different bytes")
	}
	// The shared init segment must serve for both.
	iw := executeRequest(srv.handleInit, http.MethodGet,
		"/v1/channels/cmaf/init/v0.mp4", map[string]string{"channelID": "cmaf", "repID": "v0"})
	if iw.Code != http.StatusOK {
		t.Errorf("init.mp4: got %d, want 200", iw.Code)
	}
}

// TestCMAFDiscontinuitySurvivesRestart proves task-23 persistence for fMP4:
// tfdt restores timing on restart, but the discontinuity flag lives only in the
// sidecar. With the origin gone, the restored playlist must still carry it.
func TestCMAFDiscontinuitySurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	origin := startCMAFOriginOpts(t, 4, true, 2) // discontinuity before segment 2

	h1 := boot(t, dataDir)
	if err := h1.store.Put("cmaf", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	h1.start(t)

	// Wait for VOD.
	deadline := time.Now().Add(20 * time.Second)
	var before string
	for time.Now().Before(deadline) {
		if _, b := h1.playlist(t, "cmaf"); strings.Contains(b, "#EXT-X-ENDLIST") {
			before = b
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(before, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("discontinuity not present before restart:\n%s", before)
	}
	h1.stop("cmaf")
	origin.Close()

	// Restart with the origin gone: the discontinuity must be reconstructed
	// from the sidecar, since a .m4s file carries no discontinuity marker.
	h2 := boot(t, dataDir)
	h2.start(t)
	t.Cleanup(func() { h2.stop("cmaf") })

	code, after := h2.playlist(t, "cmaf")
	if code != http.StatusOK {
		t.Fatalf("playlist after restart: got %d", code)
	}
	if n := strings.Count(after, "#EXTINF:"); n != 4 {
		t.Fatalf("after restart %d segments, want 4:\n%s", n, after)
	}
	if !strings.Contains(after, "#EXT-X-DISCONTINUITY") {
		t.Errorf("discontinuity lost across restart (sidecar not consulted for fMP4):\n%s", after)
	}
	// The DASH output must also come back from the same rebuilt index.
	dw := executeRequest(h2.srv.handleManifest, http.MethodGet,
		"/v1/channels/cmaf/manifest.mpd", map[string]string{"channelID": "cmaf"})
	if dw.Code != http.StatusOK {
		t.Errorf("manifest.mpd after restart: got %d", dw.Code)
	}
}
