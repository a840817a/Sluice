package channel

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
)

// configForTest returns a gateway config rooted at a temp data dir, so a
// started channel writes nothing outside the test.
func configForTest(t *testing.T) gocfg.Config {
	t.Helper()
	return gocfg.Config{
		Server: gocfg.ServerConfig{BaseURL: "http://gw.test"},
		Store:  gocfg.StoreConfig{DataDir: t.TempDir()},
		Worker: gocfg.WorkerConfig{FetchWorkers: 1, MaxRetriesStatic: 1},
		Window: gocfg.WindowConfig{Depth: time.Hour},
	}
}

// deadChannel points at a port nothing listens on. Discovery fails and retries
// with backoff, which is what we want: the lifecycle is exercised without any
// real upstream, and the ingest goroutines are genuinely running.
func deadChannel() Config {
	return Config{
		MPDURL:     "http://127.0.0.1:1/index.m3u8",
		SourceType: SourceHLS,
		Enabled:    true,
	}
}

func TestManagerStartIsIdempotent(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	rt1, err := m.Start(context.Background(), "ch1", deadChannel())
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	rt2, err := m.Start(context.Background(), "ch1", deadChannel())
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if rt1 != rt2 {
		t.Error("starting the same channel twice produced two runtimes; Start must be idempotent")
	}
	if got := m.Get("ch1"); got != rt1 {
		t.Error("Get returned a different runtime than Start")
	}
}

func TestManagerStopRemovesChannel(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	if _, err := m.Start(context.Background(), "ch1", deadChannel()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Stop("ch1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := m.Get("ch1"); got != nil {
		t.Error("Get returned a runtime after Stop")
	}
	if err := m.Stop("ch1"); err == nil {
		t.Error("stopping an already-stopped channel returned nil error, want an error")
	}
	if got := m.Status(); len(got) != 0 {
		t.Errorf("Status reported %d channels after Stop, want 0", len(got))
	}
}

// Stop must return promptly.
//
// Today Stop only cancels the context and returns, so this bound is slack. It
// is asserted now because the known fix for the restart-drain bug makes Stop
// join the ingest goroutines, and joining is exactly how a deadlock gets
// introduced. Without a bound the suite would hang instead of failing, which is
// far harder to diagnose than a red test.
func TestManagerStopReturnsPromptly(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	if _, err := m.Start(context.Background(), "ch1", deadChannel()); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- m.Stop("ch1") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
}

func TestManagerStopAllClearsEveryChannel(t *testing.T) {
	m := NewManager(configForTest(t))

	for _, id := range []string{"ch1", "ch2", "ch3"} {
		if _, err := m.Start(context.Background(), id, deadChannel()); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	if got := m.Status(); len(got) != 3 {
		t.Fatalf("started 3 channels, Status reports %d", len(got))
	}

	done := make(chan struct{})
	go func() { m.StopAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAll did not return within 5s")
	}

	if got := m.Status(); len(got) != 0 {
		t.Errorf("Status reports %d channels after StopAll, want 0", len(got))
	}
	for _, id := range []string{"ch1", "ch2", "ch3"} {
		if m.Get(id) != nil {
			t.Errorf("%s still present after StopAll", id)
		}
	}
}

func TestManagerSetStoreBeforeStart(t *testing.T) {
	cfg := configForTest(t)
	store, err := NewStore(filepath.Join(cfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	m := NewManager(cfg)
	m.SetStore(store)
	t.Cleanup(m.StopAll)

	if _, err := m.Start(context.Background(), "ch1", deadChannel()); err != nil {
		t.Fatalf("start with a store attached: %v", err)
	}
	if m.Get("ch1") == nil {
		t.Error("channel not running after Start")
	}
}

func TestApplyConfigHotUpdatesRunningChannel(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	rt, err := m.Start(context.Background(), "ch1", deadChannel())
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	next := deadChannel()
	next.Title = "renamed"
	next.FetchHeaders = []Header{{Name: "X-Token", Value: "rotated"}}

	if blockers := m.ApplyConfig("ch1", next); len(blockers) != 0 {
		t.Fatalf("title+header edit reported blockers %v, want none", blockers)
	}
	if got := m.Get("ch1"); got != rt {
		t.Error("hot update replaced the runtime; the channel was restarted")
	}
	if got := rt.Config().Title; got != "renamed" {
		t.Errorf("Title = %q after hot update, want %q", got, "renamed")
	}
	if got := rt.headers.Snapshot()["X-Token"]; got != "rotated" {
		t.Errorf("X-Token = %q after hot update, want %q", got, "rotated")
	}
}

func TestApplyConfigRefusesFrozenFields(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	if _, err := m.Start(context.Background(), "ch1", deadChannel()); err != nil {
		t.Fatalf("start: %v", err)
	}

	next := deadChannel()
	next.MPDURL = "http://127.0.0.1:2/other.m3u8"

	blockers := m.ApplyConfig("ch1", next)
	if len(blockers) != 1 || blockers[0] != "mpd_url" {
		t.Fatalf("blockers = %v, want [mpd_url]", blockers)
	}
	// A refusal must change nothing: the caller decides whether to restart.
	if got := m.Get("ch1").Config().MPDURL; got != deadChannel().MPDURL {
		t.Errorf("MPDURL = %q after a refused update, want it unchanged", got)
	}
}

func TestApplyConfigOnMissingChannel(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	blockers := m.ApplyConfig("nope", deadChannel())
	if len(blockers) != 1 || blockers[0] != NotRunning {
		t.Errorf("blockers = %v, want [%s]", blockers, NotRunning)
	}
}

// Config is swapped by the admin API while HTTP handlers read it. Under -race
// this fails outright if the atomic is ever replaced by a plain field.
func TestConfigSwapIsRaceFree(t *testing.T) {
	m := NewManager(configForTest(t))
	t.Cleanup(m.StopAll)

	rt, err := m.Start(context.Background(), "ch1", deadChannel())
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			next := deadChannel()
			next.Title = "t"
			rt.setConfig(next)
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = rt.Config().PlayReadyEnabled()
		_ = rt.Config().Title
	}
	<-done
}
