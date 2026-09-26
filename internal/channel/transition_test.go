package channel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashingest"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
)

// registerRuntime puts a minimal running channel into the manager without going
// through Start, which would need an upstream origin and an ingest pipeline.
// A source snapshot supplies the required restart artifacts without network I/O.
func registerRuntime(t *testing.T, m *Manager, id string, mode ChannelMode) *Runtime {
	t.Helper()
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rt := &Runtime{
		ID:       id,
		Index:    index.NewChannelIndex(index.PublishAVBarrier),
		Progress: progress.New(),
		cancel:   cancel,
		vodDone:  make(chan struct{}),
		ingest:   &fakeIngest{},
	}
	rt.watcher = &dashingest.Watcher{}
	rt.Source = dashSource{rt.watcher}
	seedVODSource(t, rt)
	rt.setConfig(Config{})
	driveMode(t, rt, mode)

	m.mu.Lock()
	m.runtimes[id] = rt
	m.mu.Unlock()
	return rt
}

func managerForTransition(t *testing.T) *Manager {
	t.Helper()
	return newTestManager(t, gocfg.Config{Store: gocfg.StoreConfig{DataDir: t.TempDir()}})
}

func TestTransitionToVODRefusesWhatItCannotTransition(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, m *Manager)
		wantErr string
	}{
		{
			name:    "channel not running",
			setup:   func(*testing.T, *Manager) {},
			wantErr: "not running",
		},
		{
			// Already finished. Transitioning again would rebuild the index and
			// re-run the VOD callback for a channel that has nothing new.
			name: "already VOD",
			setup: func(t *testing.T, m *Manager) {
				registerRuntime(t, m, "c", ModeVOD)
			},
			wantErr: "not in live or static-ingesting mode",
		},
		{
			// A transition is already under way; a second one would race it.
			name: "already transitioning",
			setup: func(t *testing.T, m *Manager) {
				registerRuntime(t, m, "c", ModeTransitioning)
			},
			wantErr: "not in live or static-ingesting mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := managerForTransition(t)
			tc.setup(t, m)

			err := m.TransitionToVOD("c")
			if err == nil {
				t.Fatal("TransitionToVOD succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// Both modes an operator can legitimately cut short: a live channel, and one
// whose static upstream is still downloading.
func TestTransitionToVODAcceptsLiveAndStaticIngesting(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode ChannelMode
	}{
		{"live", ModeLive},
		{"static ingesting", ModeStaticIngesting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := managerForTransition(t)
			rt := registerRuntime(t, m, "c", tc.mode)

			if err := m.TransitionToVOD("c"); err != nil {
				t.Fatalf("TransitionToVOD: %v", err)
			}
			waitForMode(t, rt, ModeVOD, 5*time.Second)

			if rt.VODIndex() == nil {
				t.Error("channel reached VOD mode without a full-history index")
			}
			select {
			case <-rt.vodDone:
			default:
				t.Error("vodDone was not closed, so anything waiting on the transition still is")
			}
		})
	}
}

// TransitionToVOD is the one place a goroutine is added to a channel from
// outside that channel's own goroutines, so it holds the read lock across the
// check and the Add. Stop removes the runtime under the write lock before it
// waits, which is what makes an Add racing a Wait impossible.
//
// That reasoning lived only in a comment. Run this under -race: a transition
// that slipped past the lock would either add to a WaitGroup already being
// waited on, or be reported as a data race.
func TestTransitionToVODRacesStopSafely(t *testing.T) {
	for i := 0; i < 50; i++ {
		m := managerForTransition(t)
		registerRuntime(t, m, "c", ModeLive)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Either outcome is correct: the transition started before Stop
			// detached the runtime, or it refuses because the runtime is gone.
			_ = m.TransitionToVOD("c")
		}()
		go func() {
			defer wg.Done()
			_ = m.Stop("c")
		}()
		wg.Wait()

		if got := m.Get("c"); got != nil {
			t.Fatalf("iteration %d: channel still registered after Stop", i)
		}
	}
}

func seedVODSource(t *testing.T, rt *Runtime) {
	t.Helper()
	raw := []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT0S"><Period id="0"/></MPD>`)
	parsed, err := imdp.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	rt.watcher.SetParsedMPD(parsed, raw)
}
