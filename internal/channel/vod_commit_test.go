package channel

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVODConfigFailureCompletesWithWarning(t *testing.T) {
	m := managerForTransition(t)
	path := filepath.Join(m.gwCfg.Store.DataDir, "channels.json")
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put("ch", Config{}); err != nil {
		t.Fatal(err)
	}
	m.store = st
	rt := registerRuntime(t, m, "ch", ModeLive)
	var calls atomic.Int32
	m.SetOnVODReady(func(*Runtime) { calls.Add(1) })
	// Force the configuration rename to fail after required artifacts are saved.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionToVOD("ch"); err != nil {
		t.Fatal(err)
	}
	waitMode(t, rt, ModeVOD)
	rt.wg.Wait()
	// A failed store write only costs VOD mode across a restart, which is
	// what it cost before completion waited for required artifacts.
	if calls.Load() != 1 || !rt.Progress.Snapshot().IngestComplete {
		t.Fatal("config failure did not publish exactly once")
	}
	status, _ := runningStatus(m, "ch")
	if len(status.VODWarnings) != 1 || !strings.Contains(status.VODWarnings[0], "persist VOD state") {
		t.Fatalf("vod_warnings = %q, want the store failure", status.VODWarnings)
	}
	if cfg, _ := st.Get("ch"); cfg.IsVOD {
		t.Fatal("failed rename committed VOD config")
	}
	for _, name := range []string{vodManifestFile, vodUpstreamFile} {
		if _, err := os.Stat(filepath.Join(m.vodArtifactDir("ch"), name)); err != nil {
			t.Fatal(err)
		}
	}
}

func waitMode(t *testing.T, rt *Runtime, want ChannelMode) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rt.Mode() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mode = %v, want %v", rt.Mode(), want)
}
