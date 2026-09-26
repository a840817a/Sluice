package httpapi

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// Period and representation IDs are whatever the origin's MPD says, and the
// gateway stores segments in directories named after them (ADR-0006). A period
// with no id attribute must still survive the VOD rebuild from disk, and no ID
// may place a file outside the data directory.
func TestDASHOriginIDsStayPlayableAndContained(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"period without id", ` id="0"`, ``},
		{"period id that climbs out", `id="0"`, `id="../../../escaped"`},
		{"representation id that climbs out", `id="v1"`, `id="../../../../../escaped"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := startDASHOrigin(t, strings.Replace(clearDASHMPD(), tc.from, tc.to, 1))
			root := t.TempDir()
			cfg := config.Config{
				Server: config.ServerConfig{BaseURL: "http://gw.test"},
				Store:  config.StoreConfig{DataDir: filepath.Join(root, "data")},
				Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
				Window: config.WindowConfig{Depth: time.Hour},
			}
			mgr := newTestManager(t, cfg)
			if _, err := createChannel(mgr, context.Background(), "d", ch.Config{
				MPDURL: origin.URL + "/manifest.mpd", Enabled: true,
			}); err != nil {
				t.Fatalf("start: %v", err)
			}
			t.Cleanup(func() { _ = mgr.Stop("d") })
			h := periodRouter(t, cfg, mgr)

			// The static source ingests fully and enters VOD, which rebuilds
			// the index from the directories on disk.
			deadline := time.Now().Add(15 * time.Second)
			for mgr.Get("d").Mode() != ch.ModeVOD && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if got := mgr.Get("d").Mode(); got != ch.ModeVOD {
				t.Fatalf("mode = %s, want vod", got)
			}

			_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && !strings.HasPrefix(p, cfg.Store.DataDir+string(filepath.Separator)) {
					t.Errorf("file written outside the data directory: %s", p)
				}
				return nil
			})

			code, body := get(t, h, "/v1/channels/d/media/v0.m3u8")
			pl := string(body)
			if n := strings.Count(pl, "#EXTINF:"); code != http.StatusOK || n != 3 {
				t.Fatalf("VOD v0 playlist: code %d with %d segments, want 200 with 3:\n%s", code, n, pl)
			}
			uris := regexp.MustCompile(`http://gw\.test(/v1/\S+?\.(?:m4s|mp4))`).FindAllStringSubmatch(pl, -1)
			if len(uris) != 4 { // EXT-X-MAP plus three segments
				t.Fatalf("found %d init and segment URIs, want 4:\n%s", len(uris), pl)
			}
			for _, m := range uris {
				if code, body := get(t, h, m[1]); code != http.StatusOK || len(body) == 0 {
					t.Errorf("GET %s = %d with %d bytes, want 200 with the stored file", m[1], code, len(body))
				}
			}
		})
	}
}
