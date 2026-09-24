package channel

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
)

// vodTransition is one attempt at the VOD transition, from a successful claim
// to its end. The caller claims first, through the channel's mode machine, so
// that an automatic completion and an operator's request have one winner; the
// attempt then owns every step after that, in order:
//
//  1. Bring ingest to rest through ingestControl: stop admission, drain what
//     was already admitted, join the pool and processor. The disk rebuild must
//     see every task accepted before the transition was requested.
//  2. Publish: checkpoint media, rebuild the full-history index from disk,
//     checkpoint the HLS source and keys, capture and persist artifacts, commit
//     the VOD config, and enter VOD.
//  3. Write optional exports and call the completion observer.
//
// Cancellation is observed only in step 1. If ctx ends there the attempt is
// abandoned and nothing is published. Once publication has begun it completes,
// so Stop joins a VOD being committed rather than leaving half of one behind.
// The key checkpoint is the one exception: it goes to the network and is
// bounded by ctx and keyCheckpointTimeout, and a failure is only a warning.
//
// Per ADR-0004 a failed required artifact is a warning, not a failure: the
// warnings of one attempt accumulate and are recorded once, replacing those
// of any earlier attempt. Only a failed rebuild or capture fails the attempt,
// which leaves the channel in VOD failed for an operator to retry. A retry is
// a new attempt.
type vodTransition struct {
	m  *Manager
	rt *Runtime
}

func newVODTransition(m *Manager, rt *Runtime) *vodTransition {
	return &vodTransition{m: m, rt: rt}
}

// vodOutcome is how an attempt ended.
type vodOutcome int

const (
	// vodAbandoned: ctx ended before publication; nothing was written and the
	// mode is left as claimed, because the runtime is being stopped.
	vodAbandoned vodOutcome = iota
	// vodFailed: the rebuild or the capture failed; the channel is VOD failed.
	vodFailed
	// vodCompleted: the channel is VOD.
	vodCompleted
)

// Run performs the attempt. ctx is the runtime's lifecycle context.
func (t *vodTransition) Run(ctx context.Context) vodOutcome {
	finalMPD, ok := t.quiesce(ctx)
	if !ok {
		return vodAbandoned
	}
	return t.publish(finalMPD)
}

// quiesce brings ingest to rest. It reports false if ctx ended first.
func (t *vodTransition) quiesce(ctx context.Context) (*imdp.ParsedMPD, bool) {
	in := t.rt.ingest
	finalMPD, ok := in.StopAdmission(ctx)
	if !ok || !in.AwaitDrain(ctx) {
		return nil, false
	}
	logStaticIngestShortfall(t.rt)
	if !in.Join(ctx) {
		return nil, false
	}
	return finalMPD, true
}

// publish rebuilds a full-history index from disk and publishes it. It does
// not observe cancellation; see vodTransition.
func (t *vodTransition) publish(finalMPD *imdp.ParsedMPD) vodOutcome {
	m, rt := t.m, t.rt
	if finalMPD != nil && finalMPD.MediaPresentationDuration > 0 {
		rt.finalDuration.Store(int64(finalMPD.MediaPresentationDuration))
	}

	// Every exit below records this attempt's warnings.
	warnings := m.checkpointVODMedia(rt)

	// Rebuild from disk with unbounded ring buffers so the VOD covers the whole
	// recorded history rather than just what the live ring still held. fMP4
	// timing comes from each segment's tfdt box and MPEG-TS timing from the
	// per-representation sidecar, so both formats reconstruct fully.
	vodCI := index.NewChannelIndex()
	if err := index.RebuildFromDiskForVOD(vodCI, m.gwCfg.Store.DataDir, rt.ID); err != nil {
		slog.Error("VOD rebuild failed", "channel", rt.ID, "err", err)
		// Ingest is already stopped. Expose a retryable failure rather than
		// claiming live ingest or leaving a completed attempt "transitioning".
		// The checkpoint's warnings may explain the failure, so keep them.
		recordVODWarnings(rt, warnings)
		rt.modes.Fail()
		return vodFailed
	}

	// Key fetches go to the network, so they happen before configMu: a hot
	// edit must not wait on an unreachable key server.
	warnings = append(warnings, checkpointHLSSource(rt, vodCI.Snapshot())...)

	// A hot edit cannot change the configuration between manifest generation,
	// the store commit, and runtime publication. Stop never acquires configMu.
	rt.configMu.Lock()
	a, err := m.captureVODArtifacts(rt, vodCI)
	if err != nil {
		rt.configMu.Unlock()
		slog.Error("VOD artifact capture failed", "channel", rt.ID, "err", err)
		recordVODWarnings(rt, warnings)
		rt.modes.Fail()
		return vodFailed
	}
	warnings = append(warnings, a.persistDASHManifests()...)
	m.mu.RLock()
	st, cb := m.store, m.onVODReady
	m.mu.RUnlock()
	if st != nil {
		// Merge under the store's lock rather than Get+Put: an operator can
		// be saving an edit to this same channel right now, and a plain
		// read-modify-write would silently drop whichever side lost.
		merged, syncErr, saveErr := st.update(rt.ID, func(c *Config) { c.IsVOD = true })
		if saveErr != nil {
			warnings = append(warnings, fmt.Sprintf("persist VOD state: %v", saveErr))
		} else {
			// The rename happened, so the new config is committed even when
			// its directory sync failed.
			rt.setConfig(merged)
			if syncErr != nil {
				warnings = append(warnings, fmt.Sprintf("sync VOD state: %v", syncErr))
			}
		}
	}
	recordVODWarnings(rt, warnings)
	rt.enterVOD(vodCI, true)
	rt.configMu.Unlock()
	slog.Info("channel VOD ready", "id", rt.ID)

	// Optional exports use the same capture, but cannot revoke durable VOD.
	// They remain inside this counted VOD transition so Stop also joins their
	// writes.
	a.writeOptional()
	if cb != nil {
		rt.wg.Add(1)
		go func() { defer rt.wg.Done(); cb(rt) }()
	}
	return vodCompleted
}

// enterVOD makes vodCI the index the channel serves and marks its ingest
// complete. It runs at most once per runtime, whether the channel got there
// through a transition or restarted in VOD; transitioned says which, because
// only a transition changes the mode. The index is stored before the mode
// changes, so a reader never sees VOD without the index that goes with it.
func (rt *Runtime) enterVOD(vodCI *index.ChannelIndex, transitioned bool) {
	rt.vodOnce.Do(func() {
		rt.vodIndex.Store(vodCI)
		if transitioned {
			rt.modes.Complete()
		}
		// The discovery total may never have been known — a restarted channel
		// does not keep it on disk — so the stored count stands in for it.
		snap := rt.Progress.Snapshot()
		if !snap.TotalKnown {
			rt.Progress.SetExpectedTotal(snap.SegmentsStored)
		}
		rt.Progress.MarkIngestComplete()
		close(rt.vodDone)
	})
}

// recordVODWarnings logs a VOD transition attempt's warnings and publishes them
// in the channel status.
func recordVODWarnings(rt *Runtime, warnings []string) {
	for _, w := range warnings {
		slog.Warn("VOD transition step failed", "channel", rt.ID, "warning", w)
	}
	rt.vodWarnings.Store(&warnings)
}

// staticIngestShortfall reports whether a finished static ingest holds fewer
// segments than the source named, and the two counts behind that judgement.
//
// A total the source never stated (a live stream) or one the enumeration capped
// cannot be compared against, so neither counts as short.
func staticIngestShortfall(snap progress.Snapshot, dropped uint64) (short bool, stored, expected uint64) {
	stored, expected = snap.SegmentsStored, snap.ExpectedSegments
	if !snap.TotalKnown || snap.TotalCapped || expected == 0 {
		return false, stored, expected
	}
	return stored < expected || dropped > 0, stored, expected
}

func logStaticIngestShortfall(rt *Runtime) {
	if rt.Broker == nil {
		return
	}
	if short, stored, expected := staticIngestShortfall(rt.Progress.Snapshot(), rt.Broker.Dropped()); short {
		// An idle broker means no work is left, not that the work was done: a
		// task shed under load leaves the queue just as empty as a task fetched.
		// The VOD is built from what reached disk, so say plainly that it is
		// incomplete rather than let a partial asset pass for a finished one.
		slog.Error("static ingest finished short of the source's segment count",
			"channel", rt.ID,
			"stored", stored,
			"expected", expected,
			"dropped", rt.Broker.Dropped(),
		)
	}
}
