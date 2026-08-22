package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/go-chi/chi/v5"
)

// newTestServer builds a Server backed by an empty Manager (no channels running).
func newTestServer() *Server {
	mgr := ch.NewManager(config.Config{})
	return NewServer(config.Config{}, mgr)
}

// withParams injects chi URL params so chi.URLParam works without a full router.
func withParams(req *http.Request, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func executeRequest(handler http.HandlerFunc, method, path string, routeParams map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if len(routeParams) > 0 {
		req = withParams(req, routeParams)
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func TestHandleManifest_ChannelNotFound(t *testing.T) {
	srv := newTestServer()
	w := executeRequest(srv.handleManifest, http.MethodGet, "/v1/channels/noexist/manifest.mpd",
		map[string]string{"channelID": "noexist"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleSegment_ChannelNotFound(t *testing.T) {
	srv := newTestServer()
	w := executeRequest(srv.handleSegment, http.MethodGet, "/v1/channels/noexist/segments/r0/000000001.m4s",
		map[string]string{"channelID": "noexist", "repID": "r0", "segFile": "000000001.m4s"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleSegment_MalformedSegFile(t *testing.T) {
	srv := newTestServer()
	w := executeRequest(srv.handleSegment, http.MethodGet, "/v1/channels/ch1/segments/r0/notanumber.m4s",
		map[string]string{"channelID": "ch1", "repID": "r0", "segFile": "notanumber.m4s"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for malformed seg file, got %d", w.Code)
	}
}

func TestHandleInit_ChannelNotFound(t *testing.T) {
	srv := newTestServer()
	w := executeRequest(srv.handleInit, http.MethodGet, "/v1/channels/noexist/init/r0.mp4",
		map[string]string{"channelID": "noexist", "repID": "r0"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleLicense_DisabledDRM(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Store: config.StoreConfig{DataDir: dir}}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "ch1", ch.Config{
		MPDURL:              "http://example.com/live.mpd",
		PlayReadyLicenseURL: "http://example.com/pr",
		EnablePlayReady:     ch.BoolPtr(false),
		Enabled:             true,
	}); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("ch1") })

	srv := NewServer(cfg, mgr)
	w := executeRequest(srv.handleLicense("playready"), http.MethodPost, "/v1/channels/ch1/license/playready",
		map[string]string{"channelID": "ch1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled PlayReady, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChannelHealth_UnknownChannel(t *testing.T) {
	srv := newTestServer()
	w := executeRequest(srv.handleChannelHealth, http.MethodGet, "/v1/channels/noexist/health",
		map[string]string{"channelID": "noexist"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		ChannelID string `json:"channel_id"`
		Running   bool   `json:"running"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Running {
		t.Error("expected running=false for unknown channel")
	}
	if resp.ChannelID != "noexist" {
		t.Errorf("expected channel_id=noexist, got %q", resp.ChannelID)
	}
}

func TestHandleManifest_VODFallsBackToSavedManifest(t *testing.T) {
	dir := t.TempDir()
	channelID := "vod1"
	manifestPath := filepath.Join(dir, channelID, "manifest.mpd")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := []byte("<MPD type=\"static\"></MPD>")
	if err := os.WriteFile(manifestPath, want, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := config.Config{Store: config.StoreConfig{DataDir: dir}}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), channelID, ch.Config{
		MPDURL:  "http://example.com/final.mpd",
		Enabled: true,
		IsVOD:   true,
	}); err != nil {
		t.Fatalf("start vod channel: %v", err)
	}
	srv := NewServer(cfg, mgr)

	w := executeRequest(srv.handleManifest, http.MethodGet, "/v1/channels/vod1/manifest.mpd",
		map[string]string{"channelID": channelID})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.Bytes(); string(body) != string(want) {
		t.Fatalf("unexpected manifest body: %s", string(body))
	}
}
