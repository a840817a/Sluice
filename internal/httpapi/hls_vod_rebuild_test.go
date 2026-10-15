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

// startEndedDemuxedHLSOrigin serves a finished HLS asset: a video variant, a
// demuxed audio rendition and, when subs is not zero, demuxed WebVTT
// subtitles, each with its own segment count.
func startEndedDemuxedHLSOrigin(t *testing.T, video, audio, subs int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		master := "#EXTM3U\n#EXT-X-VERSION:3\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio.m3u8"` + "\n"
		inf := `#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aud"`
		if subs > 0 {
			master += `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="subs.m3u8"` + "\n"
			inf += `,SUBTITLES="subs"`
		}
		fmt.Fprint(w, master+inf+"\nvideo.m3u8\n")
	})
	media := func(prefix, ext string, n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:4.000,\n%s%d%s\n", prefix, i, ext)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
		}
	}
	mux.HandleFunc("/video.m3u8", media("v", ".ts", video))
	mux.HandleFunc("/audio.m3u8", media("a", ".ts", audio))
	mux.HandleFunc("/subs.m3u8", media("s", ".vtt", subs))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		var n int
		switch {
		case strings.HasPrefix(name, "s"):
			fmt.Sscanf(name, "s%d.vtt", &n)
			w.Header().Set("Content-Type", "text/vtt")
			w.Write(vttSegment(n))
		case strings.HasPrefix(name, "a"):
			fmt.Sscanf(name, "a%d.ts", &n)
			w.Write(tsSegment(n + 100))
		case strings.HasPrefix(name, "v"):
			fmt.Sscanf(name, "v%d.ts", &n)
			w.Write(tsSegment(n))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An HLS channel publishes each track's segments as they land, so its VOD —
// rebuilt from disk — must list the same segments: the rebuild follows the
// channel's publication policy rather than the DASH A/V barrier. Under the
// barrier a video segment with no overlapping audio segment stayed unpublished,
// so a demuxed asset whose audio ended one segment early lost its last video
// segment on entering VOD — one a player had already been served.
//
// Subtitles are rebuilt too. WebVTT carries no container timing, so like
// MPEG-TS its timing comes from the sidecar; the rebuild used to read a .vtt
// segment's start from an fMP4 tfdt box it does not have, and dropped every
// one of them, taking the subtitle track out of the VOD.
func TestHLSVODListsWhatTheLiveChannelPublished(t *testing.T) {
	for _, tc := range []struct {
		name               string
		video, audio, subs int
	}{
		{"tracks of equal length", 6, 6, 0},
		{"audio one segment short", 6, 5, 0},
		{"with subtitles", 3, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := startEndedDemuxedHLSOrigin(t, tc.video, tc.audio, tc.subs)
			cfg := config.Config{
				Server: config.ServerConfig{BaseURL: "http://gw.test"},
				Store:  config.StoreConfig{DataDir: t.TempDir()},
				Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
				Window: config.WindowConfig{Depth: time.Hour},
			}
			mgr := newTestManager(t, cfg)
			t.Cleanup(mgr.StopAll)
			if _, err := createChannel(mgr, context.Background(), "d", ch.Config{
				SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			srv := NewServer(cfg, mgr)
			count := func(rep string) int {
				w := executeRequest(srv.handleMediaPlaylist, http.MethodGet, "/v1/channels/d/media/"+rep+".m3u8",
					map[string]string{"channelID": "d", "playlist": rep + ".m3u8"})
				return strings.Count(w.Body.String(), "#EXTINF:")
			}

			deadline := time.Now().Add(15 * time.Second)
			for mgr.Get("d").Mode() != ch.ModeVOD && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if m := mgr.Get("d").Mode(); m != ch.ModeVOD {
				t.Fatalf("mode = %s, want vod", m)
			}
			for rep, want := range map[string]int{"v0": tc.video, "a0": tc.audio, "s0": tc.subs} {
				if want == 0 {
					continue
				}
				if got := count(rep); got != want {
					t.Errorf("VOD %s playlist lists %d segments, want all %d", rep, got, want)
				}
			}
		})
	}
}
