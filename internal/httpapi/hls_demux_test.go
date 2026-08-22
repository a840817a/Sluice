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

// startDemuxedHLSOrigin serves a video variant and a separate (demuxed) audio
// rendition bound to it by an EXT-X-MEDIA audio group.
func startDemuxedHLSOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n"+
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio.m3u8"`+"\n"+
			`#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aud"`+"\n"+
			"video.m3u8\n")
	})
	media := func(prefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := 0; i < segCount; i++ {
				fmt.Fprintf(&b, "#EXTINF:4.000,\n%s%d.ts\n", prefix, i)
			}
			fmt.Fprint(w, b.String())
		}
	}
	mux.HandleFunc("/video.m3u8", media("v"))
	mux.HandleFunc("/audio.m3u8", media("a"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve any v{n}.ts / a{n}.ts with distinct bytes per track+index.
		name := strings.TrimPrefix(r.URL.Path, "/")
		if !strings.HasSuffix(name, ".ts") || len(name) < 4 {
			http.NotFound(w, r)
			return
		}
		var n int
		track := name[0]
		if _, err := fmt.Sscanf(name[1:], "%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		off := 0
		if track == 'a' {
			off = 100
		}
		w.Write(tsSegment(n + off))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestDemuxedHLSAudioIngestedAndServed proves a demuxed HLS source has its
// separate audio rendition ingested into its own AdaptationSet and re-published,
// with the master binding video and audio together.
func TestDemuxedHLSAudioIngestedAndServed(t *testing.T) {
	origin := startDemuxedHLSOrigin(t, 3)

	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "dx", ch.Config{
		SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("dx") })
	srv := NewServer(cfg, mgr)

	// Wait for both tracks to publish.
	waitSegs := func(repID string) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			w := executeRequest(srv.handleMediaPlaylist, http.MethodGet,
				"/v1/channels/dx/media/"+repID+".m3u8",
				map[string]string{"channelID": "dx", "repID": repID})
			if strings.Count(w.Body.String(), "#EXTINF:") >= 3 {
				return w.Body.String()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("track %s never reached 3 segments", repID)
		return ""
	}
	videoPl := waitSegs("v0")
	audioPl := waitSegs("a0") // the demuxed audio track is ingested and served

	if !strings.Contains(videoPl, "/segments/v0/") || !strings.Contains(audioPl, "/segments/a0/") {
		t.Error("video and audio playlists must reference their own segments")
	}

	// Master binds the two together.
	mw := executeRequest(srv.handleMasterPlaylist, http.MethodGet,
		"/v1/channels/dx/master.m3u8", map[string]string{"channelID": "dx"})
	master := mw.Body.String()
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO`,
		`GROUP-ID="aud"`,
		`LANGUAGE="en"`,
		"/v1/channels/dx/media/a0.m3u8",
		`AUDIO="aud"`,
		"/v1/channels/dx/media/v0.m3u8",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}

	// Audio segments are served from their own AdaptationSet, distinct from video.
	aw := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/dx/segments/a0/0.ts",
		map[string]string{"channelID": "dx", "repID": "a0", "segFile": "0.ts"})
	if aw.Code != http.StatusOK {
		t.Fatalf("audio segment: got %d", aw.Code)
	}
	if !bytesEqual(aw.Body.Bytes(), tsSegment(100)) {
		t.Error("audio segment 0 is not the upstream audio bytes")
	}
	vw := executeRequest(srv.handleSegment, http.MethodGet,
		"/v1/channels/dx/segments/v0/0.ts",
		map[string]string{"channelID": "dx", "repID": "v0", "segFile": "0.ts"})
	if !bytesEqual(vw.Body.Bytes(), tsSegment(0)) {
		t.Error("video segment 0 is not the upstream video bytes")
	}
}
