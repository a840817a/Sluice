package httpapi

import (
	"context"
	"fmt"
	"html"
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

// Bento4's mp4dash names representations "video/avc1" and
// "audio/und/mp4a.40.2" by default, and a codec string such as "mp4a.40.2" is
// a common ID on its own. Every URL the gateway advertises for such a
// representation — in the DASH manifest, the HLS master and the HLS media
// playlists — must be one its router serves.
func TestDASHRepresentationIDsAreServed(t *testing.T) {
	for _, tc := range []struct{ name, v1, a0 string }{
		{"ids with slashes", "video/avc1", "audio/und/mp4a.40.2"},
		{"ids with dots", "v.1", "mp4a.40.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDASHRepresentationIDsAreServed(t, tc.v1, tc.a0)
		})
	}
}

func testDASHRepresentationIDsAreServed(t *testing.T, v1, a0 string) {
	mpd := strings.NewReplacer(`id="v1"`, `id="`+v1+`"`, `id="a0"`, `id="`+a0+`"`).Replace(clearDASHMPD())
	origin := startDASHOrigin(t, mpd)
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
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

	deadline := time.Now().Add(15 * time.Second)
	for mgr.Get("d").Mode() != ch.ModeVOD && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := mgr.Get("d").Mode(); got != ch.ModeVOD {
		t.Fatalf("mode = %s, want vod", got)
	}

	fetch := func(path string) string {
		t.Helper()
		code, body := get(t, h, path)
		if code != http.StatusOK || len(body) == 0 {
			t.Errorf("GET %s = %d with %d bytes, want 200 with content", path, code, len(body))
		}
		return string(body)
	}

	// DASH: each SegmentTemplate's init and first segment.
	code, body := get(t, h, "/v1/channels/d/manifest.mpd")
	if code != http.StatusOK {
		t.Fatalf("manifest: %d", code)
	}
	tmpls := regexp.MustCompile(`(?:initialization|media)="(v1/channels/[^"]+)"`).FindAllStringSubmatch(string(body), -1)
	if len(tmpls) != 6 { // three representations, init and media each
		t.Fatalf("found %d template URLs, want 6:\n%s", len(tmpls), body)
	}
	for _, m := range tmpls {
		u := html.UnescapeString(m[1])
		u = strings.Replace(u, "$Number%09d$", fmt.Sprintf("%09d", 1), 1)
		fetch("/" + u)
	}

	// HLS: every media playlist in the master, and every init and segment in
	// each of those.
	master := fetch("/v1/channels/d/master.m3u8")
	pls := regexp.MustCompile(`http://gw\.test(/v1/\S+?\.m3u8)`).FindAllStringSubmatch(master, -1)
	if len(pls) != 3 {
		t.Fatalf("found %d media playlists in the master, want 3:\n%s", len(pls), master)
	}
	for _, p := range pls {
		pl := fetch(p[1])
		uris := regexp.MustCompile(`http://gw\.test(/v1/\S+?\.(?:m4s|mp4))`).FindAllStringSubmatch(pl, -1)
		if len(uris) != 4 { // EXT-X-MAP plus three segments
			t.Errorf("%s: found %d init and segment URIs, want 4:\n%s", p[1], len(uris), pl)
		}
		for _, u := range uris {
			fetch(u[1])
		}
	}
}
