package channel

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/dashingest"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
)

// fakeSource is a Source whose answers a test sets directly.
type fakeSource struct {
	finished    atomic.Bool
	status      *SourceStatus
	persistMeta bool
	warnings    []string
	// checkpointCtx records the context Checkpoint was called with.
	checkpointCtx context.Context
}

func (f *fakeSource) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (f *fakeSource) FinishedAsset() bool           { return f.finished.Load() }
func (f *fakeSource) Status() *SourceStatus         { return f.status }
func (f *fakeSource) PersistsSegmentMeta() bool     { return f.persistMeta }
func (f *fakeSource) Checkpoint(ctx context.Context, _ index.Snapshot) []string {
	f.checkpointCtx = ctx
	return f.warnings
}

// The DASH adapter answers from the last MPD the watcher parsed.
func TestDASHSourceAnswersFromTheMPD(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mpdType      imdp.PresentationType
		wantFinished bool
		wantStatus   *SourceStatus
	}{
		{"before the first fetch", "", false, nil},
		{"live", imdp.PresentationDynamic, false, &SourceStatus{UpstreamType: "dynamic"}},
		{"finished asset", imdp.PresentationStatic, true, &SourceStatus{UpstreamType: "static"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &dashingest.Watcher{}
			if tc.mpdType != "" {
				w.SetParsedMPD(&imdp.ParsedMPD{Type: tc.mpdType}, nil)
			}
			s := dashSource{w}
			if got := s.FinishedAsset(); got != tc.wantFinished {
				t.Errorf("FinishedAsset = %v, want %v", got, tc.wantFinished)
			}
			if got := s.Status(); (got == nil) != (tc.wantStatus == nil) || got != nil && *got != *tc.wantStatus {
				t.Errorf("Status = %+v, want %+v", got, tc.wantStatus)
			}
			if s.PersistsSegmentMeta() {
				t.Error("DASH reports sidecar records; fMP4 carries its own timing")
			}
			if w := s.Checkpoint(context.Background(), index.Snapshot{}); w != nil {
				t.Errorf("DASH checkpoint = %v, want nothing to do", w)
			}
		})
	}
}

// Before discovery an HLS source knows nothing about the upstream, but its
// segments always carry sidecar records.
func TestHLSSourceBeforeDiscovery(t *testing.T) {
	s := hlsSource{w: &hlsingest.Watcher{}}
	if s.FinishedAsset() || s.Status() != nil {
		t.Errorf("FinishedAsset = %v, Status = %+v before discovery; want false, nil", s.FinishedAsset(), s.Status())
	}
	if !s.PersistsSegmentMeta() {
		t.Error("HLS does not report sidecar records; MPEG-TS timing lives there")
	}
}

// A live channel is promoted to static ingesting as soon as its source says
// the upstream is a finished asset — unless a restore suppressed auto-VOD.
func TestAwaitStaticUpstreamAsksTheSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		suppressed bool
		want       ChannelMode
	}{
		{"promotes", false, ModeStaticIngesting},
		{"suppressed stays live", true, ModeLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{}
			src.finished.Store(true)
			rt := &Runtime{ID: "c", Source: src, Progress: progress.New()}
			rt.modes.begin(ModeLive, tc.suppressed, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 3*staticUpstreamPollInterval)
			defer cancel()

			(&Manager{}).awaitStaticUpstream(ctx, rt)

			if got := rt.Mode(); got != tc.want {
				t.Errorf("mode = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStatusReportsTheSourceStatus(t *testing.T) {
	want := &SourceStatus{EndedTargets: 2, PollTargets: 3}
	rt := &Runtime{ID: "c", Index: index.NewChannelIndex(), Source: &fakeSource{status: want}}
	if got := buildStatus(rt).Source; got != want {
		t.Errorf("status source = %+v, want %+v", got, want)
	}
	rt.Source = nil
	if got := buildStatus(rt).Source; got != nil {
		t.Errorf("status source without a Source = %+v, want nil", got)
	}
}

// The VOD transition records the source's checkpoint warnings and bounds the
// checkpoint, which may be waiting on a key server.
func TestVODTransitionCheckpointsTheSource(t *testing.T) {
	m, rt, _ := claimedRuntime(t)
	src := &fakeSource{warnings: []string{"checkpoint HLS keys: unreachable"}}
	rt.Source = src

	if got := newVODTransition(m, rt).Run(context.Background()); got != vodCompleted {
		t.Fatalf("outcome = %v, want completed", got)
	}
	if w := rt.vodWarnings.Load(); w == nil || !slices.Contains(*w, "checkpoint HLS keys: unreachable") {
		t.Errorf("VOD warnings = %v, want the source's checkpoint warning", w)
	}
	if src.checkpointCtx == nil {
		t.Fatal("the source was never checkpointed")
	}
	if deadline, ok := src.checkpointCtx.Deadline(); !ok || time.Until(deadline) > keyCheckpointTimeout {
		t.Errorf("checkpoint context deadline = %v (set %v), want within %v", deadline, ok, keyCheckpointTimeout)
	}
}
