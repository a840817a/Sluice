package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/go-chi/chi/v5"
)

// newTestAdminServer creates an AdminServer backed by a temp-file store and
// an empty Manager. No channels are pre-loaded.
func newTestAdminServer(t *testing.T) *AdminServer {
	t.Helper()
	dir := t.TempDir()
	store, err := ch.NewStore(filepath.Join(dir, "channels.json"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	mgr := ch.NewManager(config.Config{
		Store: config.StoreConfig{DataDir: dir},
	})
	// The manager and the admin server share one store, exactly as
	// cmd/gateway wires them. A restart persists through the manager, so a
	// manager without the store cannot complete one.
	mgr.SetStore(store)
	return NewAdminServer(context.Background(), mgr, store)
}

func adminExec(admin *AdminServer, method, path string, body interface{}, routeParams map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")

	if len(routeParams) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range routeParams {
			rctx.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}

	w := httptest.NewRecorder()
	switch method {
	case http.MethodPost:
		admin.handleCreateChannel(w, req)
	case http.MethodPut:
		admin.handleUpdateChannel(w, req)
	case http.MethodDelete:
		admin.handleDeleteChannel(w, req)
	case http.MethodGet:
		admin.handleChannelStatus(w, req)
	}
	return w
}

// decodeUpdate reads the hot-vs-restart outcome the update and restart handlers
// report, so tests assert on the contract the admin UI actually consumes.
func decodeUpdate(t *testing.T, w *httptest.ResponseRecorder) updateResponse {
	t.Helper()
	var got updateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return got
}

// liveChannel points at a port nothing listens on, so ingest goroutines really
// run but no upstream is contacted. Mirrors channel.deadChannel.
func liveChannel() ch.Config {
	return ch.Config{
		MPDURL:     "http://127.0.0.1:1/index.m3u8",
		SourceType: ch.SourceHLS,
		Enabled:    true,
	}
}

func TestHandleUpdateChannel_HotAppliesWithoutRestart(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := admin.store.Put("ch1", liveChannel()); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rt, err := admin.manager.Start(context.Background(), "ch1", liveChannel())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(admin.manager.StopAll)

	body := map[string]interface{}{
		"mpd_url":               liveChannel().MPDURL, // unchanged
		"source_type":           "hls",
		"title":                 "renamed",
		"playready_license_url": "http://drm.example.com/pr",
		"fetch_headers":         []map[string]string{{"name": "X-Token", "value": "rotated"}},
	}
	w := adminExec(admin, http.MethodPut, "/admin/api/channels/ch1", body,
		map[string]string{"channelID": "ch1"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	got := decodeUpdate(t, w)
	if got.Restarted {
		t.Errorf("restarted = true for a title/header-only edit, want false (fields: %v)", got.RestartFields)
	}
	if now := admin.manager.Get("ch1"); now != rt {
		t.Error("runtime was replaced; the channel was restarted despite a hot-applicable edit")
	}
	if now := rt.Config().Title; now != "renamed" {
		t.Errorf("runtime Title = %q, want %q", now, "renamed")
	}
	stored, _ := admin.store.Get("ch1")
	if stored.Title != "renamed" {
		t.Errorf("stored Title = %q, want %q — a hot update must still persist", stored.Title, "renamed")
	}
}

func TestHandleUpdateChannel_RestartsOnFrozenField(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := admin.store.Put("ch1", liveChannel()); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rt, err := admin.manager.Start(context.Background(), "ch1", liveChannel())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(admin.manager.StopAll)

	body := map[string]interface{}{
		"mpd_url":     "http://127.0.0.1:2/other.m3u8",
		"source_type": "hls",
	}
	w := adminExec(admin, http.MethodPut, "/admin/api/channels/ch1", body,
		map[string]string{"channelID": "ch1"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	got := decodeUpdate(t, w)
	if !got.Restarted {
		t.Error("restarted = false after changing mpd_url, want true")
	}
	if len(got.RestartFields) != 1 || got.RestartFields[0] != "mpd_url" {
		t.Errorf("restart_fields = %v, want [mpd_url]", got.RestartFields)
	}
	if now := admin.manager.Get("ch1"); now == rt {
		t.Error("runtime unchanged; the channel was not actually restarted")
	}
}

// A channel present in the store but not running must be started by an update,
// not reported as a hot apply against a runtime that does not exist.
func TestHandleUpdateChannel_StartsStoppedChannel(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := admin.store.Put("ch1", liveChannel()); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	t.Cleanup(admin.manager.StopAll)

	if admin.manager.Get("ch1") != nil {
		t.Fatal("channel should not be running yet")
	}
	body := map[string]interface{}{"mpd_url": liveChannel().MPDURL, "source_type": "hls", "title": "x"}
	w := adminExec(admin, http.MethodPut, "/admin/api/channels/ch1", body,
		map[string]string{"channelID": "ch1"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if admin.manager.Get("ch1") == nil {
		t.Error("channel still not running after update")
	}
	got := decodeUpdate(t, w)
	if !got.Restarted {
		t.Error("restarted = false, want true when the channel had to be started")
	}
	// not_running is an internal sentinel, not a field the operator edited.
	if len(got.RestartFields) != 0 {
		t.Errorf("restart_fields = %v, want empty for a channel that was merely stopped", got.RestartFields)
	}
}

func TestHandleRestartChannel(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := admin.store.Put("ch1", liveChannel()); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rt, err := admin.manager.Start(context.Background(), "ch1", liveChannel())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(admin.manager.StopAll)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/channels/ch1/restart", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channelID", "ch1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	admin.handleRestartChannel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if now := admin.manager.Get("ch1"); now == rt {
		t.Error("runtime unchanged; restart did not take effect")
	}
	if got := decodeUpdate(t, w); !got.Restarted {
		t.Error("restarted = false from the restart endpoint")
	}
}

func TestHandleRestartChannel_NotFound(t *testing.T) {
	admin := newTestAdminServer(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/channels/nope/restart", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channelID", "nope")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	admin.handleRestartChannel(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleCreateChannel_InvalidChannelID(t *testing.T) {
	admin := newTestAdminServer(t)
	w := adminExec(admin, http.MethodPost, "/admin/api/channels",
		map[string]interface{}{"id": "bad id!", "mpd_url": "http://example.com/stream.mpd"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid channel ID, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCreateChannel_InvalidURLScheme(t *testing.T) {
	admin := newTestAdminServer(t)
	w := adminExec(admin, http.MethodPost, "/admin/api/channels",
		map[string]interface{}{"id": "testch", "mpd_url": "file:///etc/passwd"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for file:// scheme, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCreateChannel_EmptyURL(t *testing.T) {
	admin := newTestAdminServer(t)
	w := adminExec(admin, http.MethodPost, "/admin/api/channels",
		map[string]interface{}{"id": "testch", "mpd_url": ""}, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty mpd_url, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCreateChannel_DuplicateID(t *testing.T) {
	dir := t.TempDir()
	store, err := ch.NewStore(filepath.Join(dir, "channels.json"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	_ = store.Put("dup", ch.Config{MPDURL: "http://example.com/a.mpd", Enabled: true})
	mgr := ch.NewManager(config.Config{Store: config.StoreConfig{DataDir: dir}})
	mgr.SetStore(store)
	admin := NewAdminServer(context.Background(), mgr, store)

	w := adminExec(admin, http.MethodPost, "/admin/api/channels",
		map[string]interface{}{"id": "dup", "mpd_url": "http://example.com/b.mpd"}, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate channel, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteChannel_NotFound(t *testing.T) {
	admin := newTestAdminServer(t)
	w := adminExec(admin, http.MethodDelete, "/admin/api/channels/ghost",
		nil, map[string]string{"channelID": "ghost"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChannelStatus_NotFound(t *testing.T) {
	admin := newTestAdminServer(t)
	w := adminExec(admin, http.MethodGet, "/admin/api/channels/ghost/status",
		nil, map[string]string{"channelID": "ghost"})
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateChannel_NotFound(t *testing.T) {
	admin := newTestAdminServer(t)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channelID", "ghost")
	req := httptest.NewRequest(http.MethodPut, "/admin/api/channels/ghost",
		bytes.NewBufferString(`{"mpd_url":"http://example.com/x.mpd"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	admin.handleUpdateChannel(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateChannel_PreservesInternalVODState(t *testing.T) {
	admin := newTestAdminServer(t)
	oldCfg := ch.Config{
		MPDURL:              "http://example.com/original.mpd",
		PlayReadyLicenseURL: "http://example.com/pr",
		WidevineLicenseURL:  "http://example.com/wv",
		Enabled:             true,
		IsVOD:               true,
	}
	if err := admin.store.Put("vodch", oldCfg); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	body := map[string]interface{}{
		"mpd_url":               "http://example.com/updated.mpd",
		"playready_license_url": "http://example.com/pr2",
		"widevine_license_url":  "http://example.com/wv2",
		"enable_playready":      false,
		"enable_widevine":       true,
	}
	w := adminExec(admin, http.MethodPut, "/admin/api/channels/vodch", body,
		map[string]string{"channelID": "vodch"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	updated, ok := admin.store.Get("vodch")
	if !ok {
		t.Fatal("updated channel missing from store")
	}
	if !updated.IsVOD {
		t.Fatal("expected IsVOD to be preserved on update")
	}
	if updated.PlayReadyEnabled() {
		t.Fatal("expected PlayReady to be disabled")
	}
	if !updated.WidevineEnabled() {
		t.Fatal("expected Widevine to be enabled")
	}
	rt := admin.manager.Get("vodch")
	if rt == nil {
		t.Fatal("expected updated runtime to be running")
	}
	if rt.Mode() != ch.ModeVOD {
		t.Fatalf("expected runtime to restore as VOD, got %v", rt.Mode())
	}
}
