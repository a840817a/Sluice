package httpapi

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
)

// Exercise the artifact lifecycle through real ingest and a fresh manager.
// Directories at output filenames cause real write failures without depending
// on permissions (which behave differently when tests run as root).
func TestDASHArtifactsSurviveRestartWithOriginOffline(t *testing.T) {
	// A blocked artifact must not stop the channel completing as VOD. A
	// required one is reported as a warning; the optional local export is
	// isolated and reports nothing. "upstream_removed" writes everything, then
	// loses the upstream snapshot before the restart, as a VOD written by an
	// older gateway can: restore then serves the saved manifest.
	for _, name := range []string{"all_outputs", "local.mpd", "upstream.mpd", "manifest.mpd", "upstream_removed"} {
		blocked := name
		if name == "all_outputs" || name == "upstream_removed" {
			blocked = ""
		}
		t.Run(name, func(t *testing.T) {
			origin := startClearDASHOrigin(t)
			dataDir := t.TempDir()
			const id = "artifacts"
			if blocked != "" {
				if err := os.MkdirAll(filepath.Join(dataDir, id, blocked), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			h1 := boot(t, dataDir)
			t.Cleanup(h1.mgr.StopAll)
			done := make(chan struct{})
			h1.mgr.SetOnVODReady(func(*ch.Runtime) { close(done) })
			if err := h1.store.Put(id, ch.Config{MPDURL: origin.URL + "/manifest.mpd", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			h1.start(t)
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("VOD completion observer did not run (mode %v)", h1.mgr.Get(id).Mode())
			}
			info, _ := h1.mgr.Channel(id)
			status := info.Status
			if status.Mode != "vod" {
				t.Fatalf("mode = %q, want vod", status.Mode)
			}
			wantWarning := blocked == "manifest.mpd" || blocked == "upstream.mpd"
			if got := len(status.VODWarnings) > 0; got != wantWarning {
				t.Fatalf("vod_warnings = %q, want warning: %v", status.VODWarnings, wantWarning)
			}
			h1.stop(id)
			for _, file := range []string{"manifest.mpd", "local.mpd", "upstream.mpd"} {
				if file == blocked {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dataDir, id, file))
				if err != nil || len(data) == 0 {
					t.Fatalf("artifact %s missing or empty: %v", file, err)
				}
			}
			noUpstream := blocked == "upstream.mpd" || name == "upstream_removed"
			if name == "upstream_removed" {
				if err := os.Remove(filepath.Join(dataDir, id, "upstream.mpd")); err != nil {
					t.Fatal(err)
				}
			}
			origin.Close()
			h2 := boot(t, dataDir)
			t.Cleanup(h2.mgr.StopAll)
			h2.start(t)
			w := executeRequest(h2.srv.handleManifest, http.MethodGet,
				"/v1/channels/"+id+"/manifest.mpd", map[string]string{"channelID": id})
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `type="static"`) {
				t.Fatalf("manifest after offline restart: %d %s", w.Code, w.Body.String())
			}
			if !noUpstream && h2.mgr.Get(id).Serving().ParsedMPD == nil {
				t.Fatal("saved upstream MPD was not restored")
			}
			if noUpstream {
				saved, err := os.ReadFile(filepath.Join(dataDir, id, "manifest.mpd"))
				if err != nil || !bytes.Equal(saved, w.Body.Bytes()) {
					t.Fatal("missing upstream MPD did not fall back to the saved served manifest")
				}
			}
			init := executeRequest(h2.srv.handleInit, http.MethodGet,
				"/v1/channels/"+id+"/init/0/v0.mp4", map[string]string{"channelID": id, "periodID": "0", "repID": "v0"})
			if init.Code != http.StatusOK || init.Body.Len() == 0 {
				t.Fatalf("init unavailable after restart: %d", init.Code)
			}
			for _, file := range []string{"1.m4s", "2.m4s", "3.m4s"} {
				w := executeRequest(h2.srv.handleSegment, http.MethodGet,
					"/v1/channels/"+id+"/segments/0/v0/"+file,
					map[string]string{"channelID": id, "periodID": "0", "repID": "v0", "segFile": file})
				if w.Code != http.StatusOK || w.Body.Len() == 0 {
					t.Fatalf("segment %s unavailable after restart: %d", file, w.Code)
				}
			}
		})
	}
}

func TestHLSArtifactFailureDoesNotPublishMaster(t *testing.T) {
	origin := startVODHLSOrigin(t, 2)
	dataDir := t.TempDir()
	const id = "hlsartifacts"
	// The second rendition fails after the first has been written. A master
	// must not advertise an incomplete set of media playlists.
	if err := os.MkdirAll(filepath.Join(dataDir, id, "local_1.m3u8"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := boot(t, dataDir)
	t.Cleanup(h.mgr.StopAll)
	done := make(chan struct{})
	h.mgr.SetOnVODReady(func(*ch.Runtime) { close(done) })
	if err := h.store.Put(id, ch.Config{SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("VOD completion observer did not run")
	}
	if data, err := os.ReadFile(filepath.Join(dataDir, id, "local_0.m3u8")); err != nil || len(data) == 0 {
		t.Fatalf("first rendition was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, id, "local.m3u8")); !os.IsNotExist(err) {
		t.Fatalf("master published despite incomplete media playlists: %v", err)
	}
	if code, body := h.playlist(t, id); code != http.StatusOK || !strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Fatalf("optional artifact failure broke serving: %d %s", code, body)
	}
}
