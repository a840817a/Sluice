package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// startDemuxedCMAFOrigin serves a fMP4 video variant plus a separate fMP4 audio
// rendition, each with its own init segment.
func startDemuxedCMAFOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:6\n"+
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio.m3u8"`+"\n"+
			`#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.42c01e,mp4a.40.2",AUDIO="aud"`+"\n"+
			"video.m3u8\n")
	})
	media := func(prefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
			b.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=%q\n", prefix+"init.mp4"))
			for i := 0; i < segCount; i++ {
				fmt.Fprintf(&b, "#EXTINF:4.000,\n%s%d.m4s\n", prefix, i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
		}
	}
	mux.HandleFunc("/video.m3u8", media("v"))
	mux.HandleFunc("/audio.m3u8", media("a"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(name, "init.mp4"):
			w.Write(fmp4Init(cmafTimescale))
		case strings.HasSuffix(name, ".m4s"):
			var n int
			fmt.Sscanf(name[1:], "%d.m4s", &n)
			w.Write(fmp4Segment(uint64(n) * 4 * cmafTimescale))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestDemuxedCMAFManifestHasAudioAS proves a demuxed CMAF channel's DASH
// manifest carries both a video and an audio AdaptationSet, with split codecs.
func TestDemuxedCMAFManifestHasAudioAS(t *testing.T) {
	origin := startDemuxedCMAFOrigin(t, 3)
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "ca", ch.Config{
		SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("ca") })
	srv := NewServer(cfg, mgr)

	// Wait for VOD (source ends with ENDLIST) so the manifest is static.
	deadline := time.Now().Add(20 * time.Second)
	var mpd string
	for time.Now().Before(deadline) {
		w := executeRequest(srv.handleManifest, http.MethodGet,
			"/v1/channels/ca/manifest.mpd", map[string]string{"channelID": "ca"})
		mpd = w.Body.String()
		if w.Code == http.StatusOK && strings.Contains(mpd, `mimeType="audio/mp4"`) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, want := range []string{
		`mimeType="video/mp4"`,
		`mimeType="audio/mp4"`,
		`codecs="avc1.42c01e"`, // video AS: video codec only
		`codecs="mp4a.40.2"`,   // audio AS: audio codec only
		"v1/channels/ca/segments/v0/",
		"v1/channels/ca/segments/a0/",
	} {
		if !strings.Contains(mpd, want) {
			t.Errorf("demuxed CMAF manifest missing %q:\n%s", want, mpd)
		}
	}
}
