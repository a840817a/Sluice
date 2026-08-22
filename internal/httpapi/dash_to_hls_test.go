package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

const dashTimescale = 90000
const dashSegDur = 4 * dashTimescale // 4s segments

// clearDASHMPD is a static (VOD) clear MPD: one video AS with two reps and one
// audio AS, each rep a number-based SegmentTemplate. 12s / 4s = 3 segments.
func clearDASHMPD() string {
	repTmpl := func(id string) string {
		return fmt.Sprintf(`<Representation id="%s" width="%d" height="%d" bandwidth="%d" codecs="avc1.4d401f">
        <SegmentTemplate media="%s_$Number%%09d$.m4s" initialization="%s_init.mp4" timescale="%d" duration="%d" startNumber="1"/>
      </Representation>`, id, 1280, 720, 2000000, id, id, dashTimescale, dashSegDur)
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-main:2011" mediaPresentationDuration="PT12S">
  <Period start="PT0S" duration="PT12S" id="0">
    <AdaptationSet mimeType="video/mp4" segmentAlignment="true">
      ` + repTmpl("v0") + `
      <Representation id="v1" width="640" height="360" bandwidth="800000" codecs="avc1.4d401e">
        <SegmentTemplate media="v1_$Number%09d$.m4s" initialization="v1_init.mp4" timescale="90000" duration="360000" startNumber="1"/>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="en" segmentAlignment="true">
      <Representation id="a0" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentTemplate media="a0_$Number%09d$.m4s" initialization="a0_init.mp4" timescale="90000" duration="360000" startNumber="1"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
}

// startClearDASHOrigin serves the MPD plus fMP4 init/segments for every rep.
func startClearDASHOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, clearDASHMPD())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(name, "_init.mp4"):
			w.Write(fmp4Init(dashTimescale))
		case strings.HasSuffix(name, ".m4s"):
			// name like v0_000000001.m4s → segment N (1-based).
			var rep string
			var n int
			if _, err := fmt.Sscanf(name, "%2s_%09d.m4s", &rep, &n); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(fmp4Segment(uint64(n-1) * dashSegDur))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestClearDASHServesHLS proves a clear fMP4 DASH source is also serveable as
// HLS: the master lists the video variants and binds the audio rendition, the
// media playlists carry EXT-X-MAP, and segments are served from the same files
// the DASH manifest uses.
func TestClearDASHServesHLS(t *testing.T) {
	origin := startClearDASHOrigin(t)
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	// A DASH channel: SourceType empty/dash, MPDURL is the MPD.
	if _, err := mgr.Start(context.Background(), "d2h", ch.Config{
		MPDURL: origin.URL + "/manifest.mpd", Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("d2h") })
	srv := NewServer(cfg, mgr)

	// Wait for the video rep to publish, then read the master.
	waitPl := func(rep string) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
				"/v1/channels/d2h/media/"+rep+".m3u8",
				map[string]string{"channelID": "d2h", "repID": rep})
			if strings.Count(w.Body.String(), "#EXTINF:") >= 1 && w.Code == http.StatusOK {
				return w.Body.String()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("rep %s never published", rep)
		return ""
	}
	vpl := waitPl("v0")
	apl := waitPl("a0")

	// Media playlists are fMP4: EXT-X-MAP + .m4s segments.
	for name, pl := range map[string]string{"v0": vpl, "a0": apl} {
		if !strings.Contains(pl, "#EXT-X-MAP:URI=") {
			t.Errorf("%s playlist missing EXT-X-MAP:\n%s", name, pl)
		}
		if !strings.Contains(pl, "/segments/"+name+"/") || !strings.Contains(pl, ".m4s") {
			t.Errorf("%s playlist must reference its .m4s segments:\n%s", name, pl)
		}
	}

	// Master lists the video variants and advertises the audio rendition.
	master := executeRequest(srv.handleMasterPlaylist, http.MethodGet,
		"/v1/channels/d2h/master.m3u8", map[string]string{"channelID": "d2h"}).Body.String()
	for _, want := range []string{
		"#EXT-X-STREAM-INF:",
		`CODECS="avc1.4d401f"`,
		"/v1/channels/d2h/media/v0.m3u8",
		"/v1/channels/d2h/media/v1.m3u8",
		`#EXT-X-MEDIA:TYPE=AUDIO`,
		`LANGUAGE="en"`,
		`AUDIO="audio"`,
		"/v1/channels/d2h/media/a0.m3u8",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}

	// A segment is served (same file the DASH manifest.mpd would point at).
	//
	// Ask for a segment the playlist actually advertises rather than a fixed
	// number. Segments are fetched by a pool of workers and published when the
	// A/V barrier lets them through, so which one lands first is not fixed —
	// hard-coding "1.m4s" here made this test fail whenever segment 2 was
	// published before segment 1.
	segFile := firstSegmentFile(t, vpl)
	sw := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/d2h/segments/v0/"+segFile,
		map[string]string{"channelID": "d2h", "repID": "v0", "segFile": segFile})
	if sw.Code != http.StatusOK {
		t.Errorf("segment %s is advertised by the playlist but not servable: got %d\nplaylist:\n%s",
			segFile, sw.Code, vpl)
	}
	// And the DASH manifest still works for the same channel.
	mw := executeRequest(srv.handleManifest, http.MethodGet,
		"/v1/channels/d2h/manifest.mpd", map[string]string{"channelID": "d2h"})
	if mw.Code != http.StatusOK {
		t.Errorf("manifest.mpd for the same DASH channel: got %d", mw.Code)
	}
}

func TestClearDASHHealthAdvertisesHLS(t *testing.T) {
	origin := startClearDASHOrigin(t)
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "d2h", ch.Config{
		MPDURL: origin.URL + "/manifest.mpd", Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("d2h") })
	srv := NewServer(cfg, mgr)

	// Wait for discovery.
	deadline := time.Now().Add(15 * time.Second)
	var alt string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleChannelHealth, http.MethodGet,
			"/v1/channels/d2h/health", map[string]string{"channelID": "d2h"})
		if strings.Contains(w.Body.String(), "alt_manifest_url") {
			alt = w.Body.String()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(alt, `"manifest_url":"/v1/channels/d2h/manifest.mpd"`) {
		t.Errorf("DASH channel primary should be manifest.mpd:\n%s", alt)
	}
	if !strings.Contains(alt, `"alt_manifest_url":"/v1/channels/d2h/master.m3u8"`) {
		t.Errorf("clear DASH health should advertise the HLS alt:\n%s", alt)
	}
}

// firstSegmentFile returns the file name of the first media segment a playlist
// advertises, e.g. "2.m4s". Tests use it so an assertion about "a segment" is
// made against one the gateway actually published, not a guessed number.
func firstSegmentFile(t *testing.T, playlist string) string {
	t.Helper()
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return path.Base(line)
	}
	t.Fatalf("playlist advertises no segments:\n%s", playlist)
	return ""
}
