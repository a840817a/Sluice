package channel

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	imdp "github.com/a840817a/sluice/internal/mpd"
)

// fakeIngest is an ingestControl whose steps succeed at once unless gated. A
// gated step blocks until its gate is closed or ctx ends, which is how a test
// holds the transition at an exact point without a network origin.
type fakeIngest struct {
	mu       sync.Mutex
	calls    []string
	gates    map[string]chan struct{}
	entered  map[string]chan struct{}
	finalMPD *imdp.ParsedMPD
}

const (
	stepStopAdmission = "StopAdmission"
	stepAwaitDrain    = "AwaitDrain"
	stepJoin          = "Join"
)

// gate makes step block, and returns a channel closed when the step is
// entered and a function that releases it.
func (f *fakeIngest) gate(step string) (entered <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gates == nil {
		f.gates = make(map[string]chan struct{})
		f.entered = make(map[string]chan struct{})
	}
	g, e := make(chan struct{}), make(chan struct{})
	f.gates[step], f.entered[step] = g, e
	var once sync.Once
	return e, func() { once.Do(func() { close(g) }) }
}

func (f *fakeIngest) step(ctx context.Context, name string) bool {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	g, e := f.gates[name], f.entered[name]
	f.mu.Unlock()
	if e != nil {
		close(e)
		f.mu.Lock()
		delete(f.entered, name) // a retry enters again without re-closing
		f.mu.Unlock()
	}
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return false
		}
	}
	return ctx.Err() == nil
}

func (f *fakeIngest) StopAdmission(ctx context.Context) (*imdp.ParsedMPD, bool) {
	if !f.step(ctx, stepStopAdmission) {
		return nil, false
	}
	return f.finalMPD, true
}

func (f *fakeIngest) AwaitDrain(ctx context.Context) bool { return f.step(ctx, stepAwaitDrain) }
func (f *fakeIngest) Join(ctx context.Context) bool       { return f.step(ctx, stepJoin) }

func (f *fakeIngest) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// claimedRuntime is a registered runtime with fake ingest, already claimed for
// a manual transition, ready for a vodTransition to run.
func claimedRuntime(t *testing.T) (*Manager, *Runtime, *fakeIngest) {
	t.Helper()
	m := managerForTransition(t)
	rt := registerRuntime(t, m, "c", ModeLive)
	in := &fakeIngest{}
	rt.ingest = in
	if from, ok := rt.modes.Claim(claimManual); !ok {
		t.Fatalf("claim refused from %s", from)
	}
	return m, rt, in
}

// Ingest reaches rest in order — admission closed, admitted work drained,
// pool and processor joined — before anything is published.
func TestVODTransitionQuiescesInOrderThenCompletes(t *testing.T) {
	m, rt, in := claimedRuntime(t)
	in.finalMPD = &imdp.ParsedMPD{MediaPresentationDuration: 90 * time.Second}

	if got := newVODTransition(m, rt).Run(context.Background()); got != vodCompleted {
		t.Fatalf("outcome = %v, want completed", got)
	}

	want := []string{stepStopAdmission, stepAwaitDrain, stepJoin}
	if got := in.Calls(); !slices.Equal(got, want) {
		t.Errorf("ingest calls = %v, want %v", got, want)
	}
	if rt.Mode() != ModeVOD || rt.VODIndex() == nil {
		t.Errorf("mode = %s, VOD index = %v; want VOD with an index", rt.Mode(), rt.VODIndex())
	}
	select {
	case <-rt.vodDone:
	default:
		t.Error("vodDone not closed")
	}
	if got := rt.FinalDuration(); got != 90*time.Second {
		t.Errorf("final duration = %v, want the final MPD's 90s", got)
	}
}

// Stop cancels the lifecycle context. Before publication that abandons the
// attempt: no later ingest step runs, nothing is published, and the mode is
// left as claimed for the runtime being torn down.
func TestVODTransitionAbandonedWhenCancelledBeforePublication(t *testing.T) {
	steps := []string{stepStopAdmission, stepAwaitDrain, stepJoin}
	for i, blocked := range steps {
		t.Run(blocked, func(t *testing.T) {
			m, rt, in := claimedRuntime(t)
			entered, release := in.gate(blocked)
			defer release()
			ctx, cancel := context.WithCancel(context.Background())

			result := make(chan vodOutcome, 1)
			go func() { result <- newVODTransition(m, rt).Run(ctx) }()
			<-entered
			cancel()

			select {
			case got := <-result:
				if got != vodAbandoned {
					t.Fatalf("outcome = %v, want abandoned", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled transition did not return")
			}
			if got := in.Calls(); !slices.Equal(got, steps[:i+1]) {
				t.Errorf("ingest calls = %v, want %v", got, steps[:i+1])
			}
			if rt.Mode() != ModeTransitioning || rt.VODIndex() != nil || rt.vodWarnings.Load() != nil {
				t.Errorf("abandoned attempt published something: mode %s, index %v, warnings %v",
					rt.Mode(), rt.VODIndex(), rt.vodWarnings.Load())
			}
		})
	}
}

// A retry runs the whole attempt again, including the ingest steps an earlier
// attempt already completed, which is why ingestControl must be idempotent.
func TestVODTransitionRetryRunsIngestStepsAgain(t *testing.T) {
	m, rt, _, blocker := failedVODRebuild(t)
	in := &fakeIngest{}
	rt.ingest = in
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	if _, ok := rt.modes.Claim(claimManual); !ok {
		t.Fatal("a failed transition could not be claimed again")
	}
	if got := newVODTransition(m, rt).Run(context.Background()); got != vodCompleted {
		t.Fatalf("retry outcome = %v, want completed", got)
	}
	if got := in.Calls(); !slices.Equal(got, []string{stepStopAdmission, stepAwaitDrain, stepJoin}) {
		t.Errorf("retry ingest calls = %v", got)
	}
}

// The automatic path drains before it claims, and the channel stays visibly
// static ingesting meanwhile. An operator claiming during that drain wins, and
// the automatic side then runs nothing.
func TestAutomaticClaimYieldsToManualDuringDrain(t *testing.T) {
	m := managerForTransition(t)
	rt := registerRuntime(t, m, "c", ModeStaticIngesting)
	in := &fakeIngest{}
	rt.ingest = in
	entered, release := in.gate(stepAwaitDrain)

	auto := make(chan struct{})
	go func() { defer close(auto); m.awaitStaticIngestComplete(rt) }()
	<-entered
	if rt.Mode() != ModeStaticIngesting {
		t.Errorf("mode while draining = %s, want static ingesting", rt.Mode())
	}
	if from, ok := rt.modes.Claim(claimManual); !ok {
		t.Fatalf("manual claim refused from %s", from)
	}
	release()
	<-auto

	if got := in.Calls(); !slices.Equal(got, []string{stepAwaitDrain}) {
		t.Errorf("ingest calls = %v; the automatic side ran a transition it did not claim", got)
	}
	if rt.Mode() != ModeTransitioning {
		t.Errorf("mode = %s, want the manual claim's transitioning", rt.Mode())
	}
}
