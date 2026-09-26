package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// startDASHChannel starts a DASH channel and returns the public router. When
// originMPD is non-empty the channel is a VOD restored with that MPD as its
// origin snapshot; otherwise it is live against an origin that never answers.
func startDASHChannel(t *testing.T, id string, originMPD []byte) http.Handler {
	t.Helper()
	dataDir := t.TempDir()
	cfg := config.Config{Store: config.StoreConfig{DataDir: dataDir}, Server: config.ServerConfig{BaseURL: "http://gw.test"}}
	chCfg := ch.Config{MPDURL: "http://127.0.0.1:1/never.mpd", Enabled: true}
	if originMPD != nil {
		if err := os.MkdirAll(filepath.Join(dataDir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, id, "upstream.mpd"), originMPD, 0o644); err != nil {
			t.Fatal(err)
		}
		chCfg.IsVOD = true
	}
	mgr := newTestManager(t, cfg)
	t.Cleanup(mgr.StopAll)
	if _, err := createChannel(mgr, context.Background(), id, chCfg); err != nil {
		t.Fatal(err)
	}
	return NewRouter(cfg, NewServer(cfg, mgr), NewAdminServer(context.Background(), mgr), UI{})
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// An encrypted DASH source cannot be re-signalled as HLS: the master playlist
// refuses it and the health endpoint does not advertise it.
func TestEncryptedDASHIsNotServedAsHLS(t *testing.T) {
	mpd, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Skipf("DRM manifest fixture unavailable: %v", err)
	}
	h := startDASHChannel(t, "drm", mpd)

	if code, _ := getBody(t, h, "/v1/channels/drm/master.m3u8"); code != http.StatusNotFound {
		t.Errorf("master.m3u8 for an encrypted DASH source = %d, want 404", code)
	}
	if _, body := getBody(t, h, "/v1/channels/drm/health"); strings.Contains(body, "alt_manifest_url") {
		t.Errorf("health advertises HLS for an encrypted DASH source:\n%s", body)
	}
}

// Before a DASH source's first MPD the channel could become servable as HLS,
// but it has nothing to serve yet, and health must not claim otherwise.
func TestDASHBeforeItsFirstMPDIsNotAdvertisedAsHLS(t *testing.T) {
	h := startDASHChannel(t, "early", nil)

	if code, _ := getBody(t, h, "/v1/channels/early/master.m3u8"); code != http.StatusServiceUnavailable {
		t.Errorf("master.m3u8 before the first MPD = %d, want 503", code)
	}
	if _, body := getBody(t, h, "/v1/channels/early/health"); strings.Contains(body, "alt_manifest_url") {
		t.Errorf("health advertises HLS before the first MPD:\n%s", body)
	}
}
