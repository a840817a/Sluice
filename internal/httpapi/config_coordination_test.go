package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/go-chi/chi/v5"
)

// Pause decoding so VOD completion occurs during an operator's edit request.
// A config read before decoding must not later overwrite the completed mode.
type pausedConfigBody struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *pausedConfigBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.reader.Read(p)
}

func TestConfigEditPreservesVODCompletedDuringRequest(t *testing.T) {
	admin := newTestAdminServer(t)
	t.Cleanup(admin.manager.StopAll)
	cfg := liveChannel()
	origin := startEmptyLiveHLSOrigin(t)
	cfg.MPDURL = origin.URL + "/master.m3u8"
	completed := make(chan struct{})
	admin.manager.SetOnVODReady(func(*ch.Runtime) { close(completed) })
	rt, err := createChannel(admin.manager, context.Background(), "edit", cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitForHLSDiscovery(t, rt)
	cfg.Title = "edited while finishing"
	cfg.FetchHeaders = []ch.Header{{Name: "X-Token", Value: "rotated"}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := &pausedConfigBody{reader: bytes.NewReader(data), started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(body.release) }) }
	defer resume()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/channels/edit", body)
	route := chi.NewRouteContext()
	route.URLParams.Add("channelID", "edit")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { admin.handleUpdateChannel(response, req); close(done) }()
	defer func() {
		resume()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("edit request did not stop")
		}
	}()
	select {
	case <-body.started:
	case <-time.After(5 * time.Second):
		t.Fatal("edit request did not reach decoding")
	}
	if err := admin.manager.TransitionToVOD("edit"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("VOD completion stalled behind the edit")
	}
	resume()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("edit did not complete")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("edit status %d: %s", response.Code, response.Body.String())
	}
	saved, ok := storeOf(t, admin).Get("edit")
	if !ok || !saved.IsVOD || saved.Title != cfg.Title {
		t.Fatalf("edit lost VOD state or operator fields: %+v", saved)
	}
	if admin.manager.Get("edit") != rt || rt.Mode() != ch.ModeVOD || !rt.Config().IsVOD {
		t.Fatal("hot edit replaced or reverted the completed VOD runtime")
	}
	if decodeUpdate(t, response).Restarted {
		t.Fatal("hot edit reported an unnecessary restart")
	}
}

// A directory in place of channels.json forces the atomic rename to fail,
// including when the test runs with permission to write every parent directory.
func failConfigPersistence(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "channels.json")
	backup := filepath.Join(dir, "channels-before-failure.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Error(err)
		}
	})
}

func TestConfigEditPersistenceFailureKeepsCommittedSettings(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "hot edit"
		if restart {
			name = "restart-required edit"
		}
		t.Run(name, func(t *testing.T) {
			admin, dir := newTestAdminServerIn(t)
			t.Cleanup(admin.manager.StopAll)
			before := liveChannel()
			before.Title = "committed"
			before.FetchHeaders = []ch.Header{{Name: "X-Token", Value: "original"}}
			rt, err := createChannel(admin.manager, context.Background(), "edit", before)
			if err != nil {
				t.Fatal(err)
			}
			failConfigPersistence(t, dir)
			next := before
			next.Title = "must not become visible"
			next.FetchHeaders = []ch.Header{{Name: "X-Token", Value: "must not rotate"}}
			if restart {
				next.MPDURL = "http://127.0.0.1:2/new.m3u8"
			}
			response := adminExec(admin, http.MethodPut, "/admin/api/channels/edit", next,
				map[string]string{"channelID": "edit"})
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("failed persistence status=%d: %s", response.Code, response.Body.String())
			}
			saved, ok := storeOf(t, admin).Get("edit")
			if !ok || !reflect.DeepEqual(saved, before) {
				t.Fatalf("failed edit changed stored settings: %+v", saved)
			}
			current := admin.manager.Get("edit")
			if current == nil || !reflect.DeepEqual(current.Config(), before) {
				t.Fatal("failed edit did not retain/restore committed runtime settings")
			}
			if !restart && current != rt {
				t.Fatal("failed hot edit restarted the channel")
			}
		})
	}
}

// An older request can finish decoding after a newer request has committed.
// Failure must retain that newer commit, never restore the older request's
// snapshot. The HTTP request ordering is deterministic here.
func TestFailedConfigEditDoesNotUndoAnotherCommittedEdit(t *testing.T) {
	admin, dir := newTestAdminServerIn(t)
	t.Cleanup(admin.manager.StopAll)
	initial := liveChannel()
	initial.Title = "initial"
	rt, err := createChannel(admin.manager, context.Background(), "edit", initial)
	if err != nil {
		t.Fatal(err)
	}
	older := initial
	older.Title = "older request that will fail"
	data, err := json.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	body := &pausedConfigBody{reader: bytes.NewReader(data), started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(body.release) }) }
	defer resume()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/channels/edit", body)
	route := chi.NewRouteContext()
	route.URLParams.Add("channelID", "edit")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { admin.handleUpdateChannel(response, req); close(done) }()
	defer func() {
		resume()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("older edit request did not stop")
		}
	}()
	select {
	case <-body.started:
	case <-time.After(5 * time.Second):
		t.Fatal("older edit did not reach decoding")
	}
	newer := initial
	newer.Title = "newer successful edit"
	newer.FetchHeaders = []ch.Header{{Name: "X-Token", Value: "newer"}}
	success := adminExec(admin, http.MethodPut, "/admin/api/channels/edit", newer,
		map[string]string{"channelID": "edit"})
	if success.Code != http.StatusOK {
		t.Fatalf("newer edit failed: %s", success.Body.String())
	}
	failConfigPersistence(t, dir)
	resume()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("older edit did not complete")
	}
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("older edit should fail persistence: %d %s", response.Code, response.Body.String())
	}
	saved, _ := storeOf(t, admin).Get("edit")
	if !reflect.DeepEqual(saved, newer) || !reflect.DeepEqual(rt.Config(), newer) {
		t.Fatalf("failed edit undid newer commit: stored=%+v runtime=%+v", saved, rt.Config())
	}
}
