package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

const testTimescale = 90000
const testSegDur = 4 * testTimescale // 4s segments

// mp4box wraps payload in an ISO-BMFF box header (size + fourcc).
//
// This file carries its own copy of the fMP4 origin fixture used by
// internal/httpapi's tests (dash_to_hls_test.go, hls_cmaf_test.go): those
// helpers live in _test.go files, so they are not importable from here.
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
// given media timescale.
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

// clearDASHMPD is a static (VOD) clear MPD: one video AdaptationSet and one
// audio AdaptationSet, each a number-based SegmentTemplate. 12s / 4s = 3
// segments, so ingest completes quickly and the channel transitions on its own.
func clearDASHMPD() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-main:2011" mediaPresentationDuration="PT12S">
  <Period start="PT0S" duration="PT12S" id="0">
    <AdaptationSet mimeType="video/mp4" segmentAlignment="true">
      <Representation id="v0" width="1280" height="720" bandwidth="2000000" codecs="avc1.4d401f">
        <SegmentTemplate media="v0_$Number%09d$.m4s" initialization="v0_init.mp4" timescale="90000" duration="360000" startNumber="1"/>
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
			w.Write(fmp4Init(testTimescale))
		case strings.HasSuffix(name, ".m4s"):
			// name like v0_000000001.m4s → segment N (1-based).
			var rep string
			var n int
			if _, err := fmt.Sscanf(name, "%2s_%09d.m4s", &rep, &n); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(fmp4Segment(uint64(n-1) * testSegDur))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// testConfig is a gateway config rooted at dataDir. Addr is unset: these tests
// exercise composition and drive the router directly, never ListenAndServe.
func testConfig(dataDir string) config.Config {
	return config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dataDir},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
}

// writeChannelsJSON seeds the store the gateway restores from at startup.
func writeChannelsJSON(t *testing.T, dataDir string, channels map[string]channel.Config) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	data, err := json.Marshal(channels)
	if err != nil {
		t.Fatalf("marshal channels.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "channels.json"), data, 0o644); err != nil {
		t.Fatalf("write channels.json: %v", err)
	}
}

// The VOD snapshot is the one piece of gateway wiring whose absence is
// invisible: drop manager.SetOnVODReady and every request still succeeds, the
// suite stays green, and the only symptom is a VOD channel that comes back
// empty after a restart.
//
// internal/httpapi's TestVODTransitionSnapshotsManifestToDisk proves
// SaveVODManifest works as a callback, but it registers the callback itself.
// This test registers nothing: it seeds channels.json, hands the config to
// newGateway, and lets the composed gateway restore and transition the channel
// on its own. The manifest on disk therefore only appears if newGateway
// registered the callback at all.
//
// It does NOT pin the registration's position relative to the restore loop:
// this channel takes ~2s to transition, so registering after the loop still
// lands in time (verified by moving the line). That ordering requirement —
// SetOnVODReady's "must be set before any Start()" — stays a code-reading
// constraint, documented at the call site in gateway.go.
func TestNewGatewayRegistersVODSnapshotCallback(t *testing.T) {
	origin := startClearDASHOrigin(t)
	dataDir := t.TempDir()
	writeChannelsJSON(t, dataDir, map[string]channel.Config{
		"gwvod": {
			MPDURL:              origin.URL + "/manifest.mpd",
			Enabled:             true,
			EnableVODTransition: true,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw, err := newGateway(ctx, testConfig(dataDir))
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	t.Cleanup(gw.manager.StopAll)

	// The restore loop started the channel; the upstream MPD is static, so the
	// channel ingests everything and transitions to VOD once the broker drains.
	if gw.manager.Get("gwvod") == nil {
		t.Fatal("newGateway did not start the enabled channel from channels.json")
	}

	manifestPath := filepath.Join(dataDir, "gwvod", "manifest.mpd")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(manifestPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no VOD manifest at %s after 30s (channel mode %v). "+
				"If ingest worked but this file is missing, newGateway is not "+
				"registering srv.SaveVODManifest via manager.SetOnVODReady "+
				"— see gateway.go.",
				manifestPath, gw.manager.Get("gwvod").Mode())
		}
		time.Sleep(100 * time.Millisecond)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read saved manifest: %v", err)
	}
	got := string(data)
	// A static manifest is what a restart needs; a dynamic one would send the
	// restored channel looking for a live edge that no longer exists.
	if !strings.Contains(got, `type="static"`) {
		t.Errorf("saved manifest is not static:\n%s", truncateForLog(got, 400))
	}
	// And it must address this gateway, not the upstream origin.
	if !strings.Contains(got, "v1/channels/gwvod/segments/") {
		t.Errorf("saved manifest does not point at this gateway:\n%s", truncateForLog(got, 400))
	}
}

// The router is the other half of composition: the embedded UI handlers are
// passed in by main, and a nil member degrades to a "not loaded" 404 rather
// than failing loudly. These assertions catch a UI handler that went missing.
func TestNewGatewayRouterServesUIAndHealth(t *testing.T) {
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw, err := newGateway(ctx, testConfig(dataDir))
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	t.Cleanup(gw.manager.StopAll)

	serve := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		gw.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	if w := serve("/healthz"); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("GET /healthz: got %d %q, want 200 \"ok\"", w.Code, w.Body.String())
	}

	// The standalone player: the real handler rewrites to the embedded
	// player.html, the placeholder would answer "player UI not loaded".
	w := serve("/player/gwvod")
	if w.Code != http.StatusOK {
		t.Errorf("GET /player/gwvod: got %d %q, want 200 — is UI.Player wired?",
			w.Code, truncateForLog(w.Body.String(), 200))
	} else if body := w.Body.String(); !strings.Contains(body, "<html") {
		t.Errorf("GET /player/gwvod did not serve the embedded player page:\n%s",
			truncateForLog(body, 200))
	}

	// The admin SPA. Requested as the directory, because http.FileServer
	// redirects an explicit /admin/index.html back to /admin/. An empty admin
	// password is what testConfig has, so basic auth matches on empty
	// credentials; without the header the route is 401 either way.
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.SetBasicAuth("", "")
	aw := httptest.NewRecorder()
	gw.handler.ServeHTTP(aw, req)
	if aw.Code != http.StatusOK {
		t.Errorf("GET /admin/: got %d %q, want 200 — is UI.Admin wired?",
			aw.Code, truncateForLog(aw.Body.String(), 200))
	} else if body := aw.Body.String(); !strings.Contains(body, "<html") {
		t.Errorf("GET /admin/ did not serve the embedded admin page:\n%s",
			truncateForLog(body, 200))
	}
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// A data directory the gateway cannot write is the one misconfiguration that
// used to survive every check: the process starts, /healthz answers 200, the
// container reports healthy, and the failure only appears when a channel first
// writes a segment — as "mkdir /data/<uuid>: permission denied", far from its
// cause. Found on a real Podman host, where the image ran as a gid the
// bind-mounted directory did not grant.
//
// newGateway must therefore refuse to build. This pins that, and that the
// refusal happens before the store is opened rather than as a side effect of it.
func TestNewGatewayRefusesAnUnwritableDataDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so this cannot be provoked")
	}
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw, err := newGateway(ctx, testConfig(dataDir))
	if err == nil {
		gw.manager.StopAll()
		t.Fatal("newGateway succeeded on an unwritable data directory; it would have reported healthy and then failed every write")
	}
	if !strings.Contains(err.Error(), dataDir) {
		t.Errorf("error does not name the directory %q: %v", dataDir, err)
	}
	// The operator's next question is always "unwritable by whom" — the answer
	// has to travel with the error, because reproducing it means a redeploy.
	for _, want := range []string{"uid=", "gid=", "mode="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}
