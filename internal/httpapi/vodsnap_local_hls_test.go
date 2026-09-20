package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// startVODHLSOrigin serves a finished two-variant HLS asset: both media
// playlists carry EXT-X-ENDLIST, so the channel ingests everything and
// transitions to VOD on its own. Two variants on purpose — the master playlist
// and the numbering of the per-representation playlists against it only mean
// anything when there is more than one.
func startVODHLSOrigin(t *testing.T, segCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.4d401e\"\n"+
			"media/v0.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=400000,RESOLUTION=320x180,CODECS=\"avc1.4d400d\"\n"+
			"media/v1.m3u8\n")
	})

	playlist := func(w http.ResponseWriter, prefix string) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := range segCount {
			fmt.Fprintf(&b, "#EXTINF:4.000,\n%sseg%d.ts\n", prefix, i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	}
	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) { playlist(w, "a") })
	mux.HandleFunc("/media/v1.m3u8", func(w http.ResponseWriter, r *http.Request) { playlist(w, "b") })

	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var kind string
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/%1sseg%d.ts", &kind, &n); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(tsSegment(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The HLS half of the local manifest: a master beside manifest.mpd, one media
// playlist per representation, and every URI in them resolving to a file on
// disk. Generating playlists that name segments which are not there is the
// failure mode this guards, the same as the DASH half.
func TestVODTransitionWritesResolvableLocalPlaylists(t *testing.T) {
	origin := startVODHLSOrigin(t, 4)

	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dataDir},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}

	mgr := ch.NewManager(cfg)
	srv := NewServer(cfg, mgr)
	mgr.SetOnVODReady(srv.SaveVODManifest)
	t.Cleanup(func() { _ = mgr.Stop("hlslocal") })

	if _, err := mgr.Start(context.Background(), "hlslocal", ch.Config{
		SourceType: ch.SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	channelDir := filepath.Join(dataDir, "hlslocal")
	masterPath := filepath.Join(channelDir, "local.m3u8")

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(masterPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no local master at %s after 30s (channel mode %v)",
				masterPath, mgr.Get("hlslocal").Mode())
		}
		time.Sleep(100 * time.Millisecond)
	}

	master, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatalf("read master: %v", err)
	}

	// Every media playlist the master names must exist, and every segment those
	// playlists name must exist.
	var mediaCount, segCount int
	for _, line := range strings.Split(string(master), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		mediaCount++
		mediaPath := filepath.Join(channelDir, filepath.FromSlash(line))
		body, err := os.ReadFile(mediaPath)
		if err != nil {
			t.Errorf("master names %q, which is not on disk: %v", line, err)
			continue
		}
		if !strings.Contains(string(body), "#EXT-X-ENDLIST") {
			t.Errorf("%s is not terminated; a finished asset must be", line)
		}
		for _, seg := range strings.Split(string(body), "\n") {
			seg = strings.TrimSpace(seg)
			if seg == "" || strings.HasPrefix(seg, "#") {
				continue
			}
			segCount++
			if filepath.IsAbs(seg) || strings.HasPrefix(seg, "http") {
				t.Errorf("%q is not a relative path", seg)
				continue
			}
			if _, err := os.Stat(filepath.Join(channelDir, filepath.FromSlash(seg))); err != nil {
				t.Errorf("%s names %q, which is not on disk: %v", line, seg, err)
			}
		}
	}

	if mediaCount != 2 {
		t.Errorf("master names %d media playlists, want 2 (one per variant):\n%s", mediaCount, master)
	}
	if segCount == 0 {
		t.Fatalf("no segments named across the media playlists")
	}
	t.Logf("master names %d playlists covering %d segments", mediaCount, segCount)
}
