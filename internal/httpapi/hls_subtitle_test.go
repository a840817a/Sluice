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

func vttSegment(n int) []byte {
	return []byte(fmt.Sprintf("WEBVTT\n\n00:00:%02d.000 --> 00:00:%02d.000\nline %d\n", n*4, n*4+4, n))
}

// startSubtitledHLSOrigin serves video + demuxed audio + demuxed WebVTT subtitles.
func startSubtitledHLSOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n"+
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio.m3u8"`+"\n"+
			`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="subs.m3u8"`+"\n"+
			`#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aud",SUBTITLES="subs"`+"\n"+
			"video.m3u8\n")
	})
	media := func(prefix, ext string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := 0; i < segCount; i++ {
				fmt.Fprintf(&b, "#EXTINF:4.000,\n%s%d%s\n", prefix, i, ext)
			}
			fmt.Fprint(w, b.String())
		}
	}
	mux.HandleFunc("/video.m3u8", media("v", ".ts"))
	mux.HandleFunc("/audio.m3u8", media("a", ".ts"))
	mux.HandleFunc("/subs.m3u8", media("s", ".vtt"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		var n int
		switch {
		case strings.HasSuffix(name, ".vtt"):
			if _, err := fmt.Sscanf(name, "s%d.vtt", &n); err != nil {
				http.NotFound(w, r)
				return
			}
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

// TestSubtitlesIngestedAndServed proves demuxed WebVTT subtitles are ingested
// into their own AdaptationSet, served as text/vtt, and advertised in the master.
func TestSubtitlesIngestedAndServed(t *testing.T) {
	origin := startSubtitledHLSOrigin(t, 3)
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "sx", ch.Config{
		SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("sx") })
	srv := NewServer(cfg, mgr)

	wait := func(rep string) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
				"/v1/channels/sx/media/"+rep+".m3u8",
				map[string]string{"channelID": "sx", "repID": rep})
			if strings.Count(w.Body.String(), "#EXTINF:") >= 3 {
				return w.Body.String()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("track %s never reached 3 segments", rep)
		return ""
	}
	wait("v0")
	wait("a0")
	subPl := wait("s0") // WebVTT subtitle track ingested
	if !strings.Contains(subPl, "/segments/s0/0.vtt") {
		t.Errorf("subtitle playlist must reference .vtt segments:\n%s", subPl)
	}

	// Master advertises audio + subtitles, and the variant binds both.
	master := executeRequest(srv.handleMasterPlaylist, http.MethodGet,
		"/v1/channels/sx/master.m3u8", map[string]string{"channelID": "sx"}).Body.String()
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO`, `#EXT-X-MEDIA:TYPE=SUBTITLES`,
		`AUDIO="aud"`, `SUBTITLES="subs"`,
		"/v1/channels/sx/media/s0.m3u8",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}

	// A subtitle segment is served as WebVTT text.
	sw := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/sx/segments/s0/0.vtt",
		map[string]string{"channelID": "sx", "repID": "s0", "segFile": "0.vtt"})
	if sw.Code != http.StatusOK {
		t.Fatalf("subtitle segment: got %d", sw.Code)
	}
	if ct := sw.Header().Get("Content-Type"); ct != "text/vtt" {
		t.Errorf("subtitle Content-Type = %q, want text/vtt", ct)
	}
	if !strings.HasPrefix(sw.Body.String(), "WEBVTT") {
		t.Errorf("subtitle segment is not WebVTT: %q", sw.Body.String())
	}
}
