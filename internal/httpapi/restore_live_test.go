package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	ch "github.com/a840817a/sluice/internal/channel"
)

// restoreExecCtx is restoreExec with control over the request's context.
func restoreExecCtx(admin *AdminServer, id string, ctx context.Context) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/api/channels/"+id+"/restore-to-live", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channelID", id)
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	admin.handleRestoreToLive(w, req)
	return w
}

// restoreExec drives handleRestoreToLive directly. adminExec dispatches on the
// HTTP method, so it cannot reach a second POST route.
func restoreExec(admin *AdminServer, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/api/channels/"+id+"/restore-to-live", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channelID", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	admin.handleRestoreToLive(w, req)
	return w
}

// deadVODChannel is a channel already marked VOD, pointing at a port nothing
// listens on: restoring it starts live ingest that fails and retries, which is
// all this test needs the runtime to do.
func deadVODChannel() ch.Config {
	return ch.Config{
		MPDURL:     "http://127.0.0.1:1/index.m3u8",
		SourceType: ch.SourceHLS,
		Enabled:    true,
		IsVOD:      true,
	}
}

func TestRestoreToLiveUnknownChannelIs404(t *testing.T) {
	admin := newTestAdminServer(t)
	if got := restoreExec(admin, "nosuch").Code; got != http.StatusNotFound {
		t.Errorf("status = %d for an unknown channel, want 404", got)
	}
}

func TestRestoreToLiveLiveChannelIs400(t *testing.T) {
	admin := newTestAdminServer(t)
	cfg := deadVODChannel()
	cfg.IsVOD = false
	if err := admin.store.Put("ch", cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := restoreExec(admin, "ch").Code; got != http.StatusBadRequest {
		t.Errorf("status = %d for a channel that is already live, want 400", got)
	}
}

// TestRestoreToLiveClearsIsVOD is the success path: the response is only worth
// anything if the stored flag actually changed, since that flag is what makes
// the next gateway start skip ingest entirely.
func TestRestoreToLiveClearsIsVOD(t *testing.T) {
	admin := newTestAdminServer(t)
	t.Cleanup(func() { admin.manager.StopAll() })

	if err := admin.store.Put("ch", deadVODChannel()); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := admin.manager.Start(context.Background(), "ch", deadVODChannel()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if got := restoreExec(admin, "ch").Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	saved, ok := admin.store.Get("ch")
	if !ok {
		t.Fatal("channel disappeared from the store")
	}
	if saved.IsVOD {
		t.Error("is_vod is still set after a 200 response")
	}
	if rt := admin.manager.Get("ch"); rt == nil || rt.Mode() != ch.ModeLive {
		t.Error("channel is not running in live mode after a restore")
	}
}

// TestRestoreToLiveOutlivesTheRequest pins which context the restored channel
// is started with. A channel outlives the HTTP response that asked for it, so
// starting it from the request's context kills its ingest the moment the
// handler returns — the restore appears to succeed and the channel silently
// fetches nothing.
//
// The request context here is cancelled before the handler runs, which is the
// worst case of the same mistake: with r.Context() the watcher never issues a
// single request, so the origin's hit count stays where it was.
func TestRestoreToLiveOutlivesTheRequest(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n"+
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.000,\nseg0.ts\n")
	}))
	defer origin.Close()

	admin := newTestAdminServer(t)
	t.Cleanup(func() { admin.manager.StopAll() })

	cfg := ch.Config{
		MPDURL:     origin.URL + "/index.m3u8",
		SourceType: ch.SourceHLS,
		Enabled:    true,
		IsVOD:      true,
	}
	if err := admin.store.Put("ch", cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := admin.manager.Start(context.Background(), "ch", cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	// A VOD channel runs no ingest, so nothing has been fetched yet.
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("origin was hit %d times before the restore", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := restoreExecCtx(admin, "ch", ctx).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the restored channel never fetched its playlist: its ingest was bound to the request context")
}
