package channel

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func managerForConfigUpdate(t *testing.T, id string, cfg Config) (*Manager, *Store, *Runtime) {
	t.Helper()
	gwCfg := configForTest(t)
	store, err := NewStore(filepath.Join(gwCfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(gwCfg)
	m.SetStore(store)
	t.Cleanup(m.StopAll)
	if err := store.Put(id, cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := m.Start(context.Background(), id, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, store, rt
}

// The old runtime finishes its VOD transition only after Stop cancels it.
// UpdateConfig must release configMu before joining that counted goroutine,
// then read the newly committed IsVOD instead of reusing its earlier snapshot.
func TestUpdateConfigRestartJoinsPendingVODTransition(t *testing.T) {
	gwCfg := configForTest(t)
	store, err := NewStore(filepath.Join(gwCfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(gwCfg)
	m.SetStore(store)
	base := deadChannel()
	base.SourceType = SourceDASH
	if err := store.Put("ch", base); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := m.Start(ctx, "ch", base)
	if err != nil {
		t.Fatal(err)
	}

	completeVODWhenStopped(t, m, rt)

	next := deadChannel()
	next.SourceType = SourceHLS // frozen in both live and VOD modes
	done := make(chan struct{})
	var result UpdateResult
	go func() {
		result, err = m.UpdateConfig(ctx, "ch", next)
		close(done)
	}()
	defer func() {
		rt.cancel()
		// A regression deadlocking UpdateConfig must fail this test, rather
		// than also hanging cleanup on its held operation mutex.
		select {
		case <-done:
			m.StopAll()
		default:
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateConfig deadlocked while Stop joined a VOD transition")
	}
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !result.Restarted {
		t.Fatal("source change did not restart the channel")
	}
	saved, ok := store.Get("ch")
	if !ok || !saved.IsVOD {
		t.Fatalf("stored config = %+v, want transition's IsVOD", saved)
	}
	got := m.Get("ch")
	if got == nil {
		t.Fatal("replacement runtime missing")
	}
	if !got.Config().IsVOD || got.Config().SourceType != SourceHLS {
		t.Fatalf("replacement runtime config = %+v, want VOD HLS", got.Config())
	}
}

// completeVODWhenStopped makes rt finish a VOD transition only once Stop
// cancels it, so the transition commits while Stop is joining it.
func completeVODWhenStopped(t *testing.T, m *Manager, rt *Runtime) {
	t.Helper()
	seedVODSource(t, rt)
	stopping := make(chan struct{})
	cancelRuntime := rt.cancel
	var once sync.Once
	rt.cancel = func() {
		cancelRuntime()
		once.Do(func() { close(stopping) })
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		<-stopping
		// This fixture models the already-quiescent publication phase. The
		// production VOD transition observes cancellation while stopping source and
		// draining the pipeline; publication itself remains joinable by Stop so
		// its store update is visible before Restart reads the configuration.
		if _, ok := rt.modes.Claim(claimManual); ok {
			newVODTransition(m, rt).publish(nil)
		}
	}()
}

// Restart reads the configuration after Stop, so a VOD transition that
// commits while Stop joins it brings the channel back as VOD, not live.
// This is a deliberate change from reading before Stop (accepted 2026-09-23).
func TestRestartAdoptsVODCompletedDuringStop(t *testing.T) {
	gwCfg := configForTest(t)
	store, err := NewStore(filepath.Join(gwCfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(gwCfg)
	m.SetStore(store)
	t.Cleanup(m.StopAll)
	base := deadChannel()
	base.SourceType = SourceDASH
	if err := store.Put("ch", base); err != nil {
		t.Fatal(err)
	}
	rt, err := m.Start(context.Background(), "ch", base)
	if err != nil {
		t.Fatal(err)
	}
	completeVODWhenStopped(t, m, rt)

	if err := m.Restart(context.Background(), "ch"); err != nil {
		t.Fatal(err)
	}
	got := m.Get("ch")
	if got == nil || got == rt {
		t.Fatal("Restart did not replace the runtime")
	}
	if !got.Config().IsVOD {
		t.Fatalf("replacement runtime config = %+v, want the VOD committed during Stop", got.Config())
	}
}

// Concurrent PUTs are whole-document updates, but each serialized completion
// must leave persistence, runtime config, and shared fetch headers aligned.
func TestUpdateConfigConcurrentEditsKeepStoreRuntimeAndHeadersAligned(t *testing.T) {
	base := deadChannel()
	base.Title = "base"
	base.FetchHeaders = []Header{{Name: "X-Token", Value: "base"}}
	m, store, rt := managerForConfigUpdate(t, "ch", base)

	a := base
	a.Title = "first"
	a.FetchHeaders = []Header{{Name: "X-Token", Value: "first"}}
	b := base
	b.Title = "second"
	b.FetchHeaders = []Header{{Name: "X-Token", Value: "second"}}

	var wg sync.WaitGroup
	for _, next := range []Config{a, b} {
		wg.Add(1)
		go func(next Config) {
			defer wg.Done()
			if _, err := m.UpdateConfig(context.Background(), "ch", next); err != nil {
				t.Errorf("UpdateConfig: %v", err)
			}
		}(next)
	}
	wg.Wait()

	saved, ok := store.Get("ch")
	if !ok {
		t.Fatal("channel disappeared from store")
	}
	if got := rt.Config(); !reflect.DeepEqual(got, saved) {
		t.Fatalf("runtime config = %+v, store config = %+v", got, saved)
	}
	if got, want := rt.headers.Snapshot(), HeaderMap(saved.FetchHeaders); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime headers = %v, want %v", got, want)
	}
}

// A hot persistence failure must leave the old credentials live. Publishing
// headers first would make a failed edit effective only until process restart.
func TestUpdateConfigFailedSaveLeavesRuntimeHeadersUnchanged(t *testing.T) {
	base := deadChannel()
	base.Title = "old"
	base.FetchHeaders = []Header{{Name: "X-Token", Value: "old"}}
	m, store, rt := managerForConfigUpdate(t, "ch", base)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blocker, "channels.json")

	next := base
	next.Title = "new"
	next.FetchHeaders = []Header{{Name: "X-Token", Value: "new"}}
	if _, err := m.UpdateConfig(context.Background(), "ch", next); err == nil {
		t.Fatal("UpdateConfig succeeded with an unwritable store path")
	}
	if got := rt.Config(); !reflect.DeepEqual(got, base) {
		t.Fatalf("runtime config = %+v, want unchanged %+v", got, base)
	}
	if got := rt.headers.Snapshot()["X-Token"]; got != "old" {
		t.Fatalf("runtime header = %q, want old credential", got)
	}
}
