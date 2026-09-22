package channel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashingest"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
)

// ChannelMode is the lifecycle state of a running channel.
type ChannelMode int32

const (
	ModeLive            ChannelMode = 0
	ModeTransitioning   ChannelMode = 1
	ModeVOD             ChannelMode = 2
	ModeStaticIngesting ChannelMode = 3 // static MPD discovered; segments downloading
)

// Runtime holds the live state for one running channel.
type Runtime struct {
	ID    string
	Index *index.ChannelIndex
	// Watcher is the DASH ingest watcher; nil for HLS channels.
	Watcher *dashingest.Watcher
	// HLS is the HLS ingest watcher; nil for DASH channels.
	HLS *hlsingest.Watcher
	// Keys caches AES-128 content keys for HLS channels; nil for DASH channels.
	Keys *hlskey.Cache
	// Source is whichever of the two is actually driving ingestion.
	Source Source
	// Progress counts what this channel has ingested since it was last started.
	// Written by the ingest goroutines, read by the admin API.
	Progress *progress.Counters
	// Broker is the channel's download queue. Held here so status can report its
	// depth and its cumulative drop count — the difference between "still
	// downloading" and "stalled because work was thrown away".
	Broker *queue.Broker
	// headers is the provider shared by Keys, Source and the fetch pool. Holding
	// it here is what lets UpdateConfig rotate upstream credentials in place.
	headers *fetch.AtomicHeaders
	// cancel ends the whole runtime. Source and pipeline have child contexts so
	// finalization can stop admitting work while allowing work already admitted
	// to finish.
	cancel       context.CancelFunc
	lifecycleCtx context.Context
	sourceCancel context.CancelFunc
	poolCancel   context.CancelFunc
	sourceDone   chan struct{} // closed after Source.Run has returned for good
	pipelineDone chan struct{} // closed after the pool and processor have exited
	// wg counts every goroutine started on this channel's behalf, so Stop can
	// join them instead of merely cancelling. Each Add happens either before
	// Start returns or inside a goroutine that is itself already counted, so it
	// can never race the Wait in Stop.
	wg sync.WaitGroup

	// cfg is the channel's configuration. It is behind an atomic because the
	// admin API swaps it while HTTP handlers read it — see Config below.
	// configMu coordinates the store commit and runtime publication with VOD
	// finalization. It is deliberately never held while stopping a runtime:
	// Stop waits for the goroutines that can need this mutex.
	configMu sync.Mutex
	cfg      atomic.Pointer[Config]
	mode     atomic.Int32 // ChannelMode
	vodOnce  sync.Once
	vodDone  chan struct{} // closed when VOD index is ready

	// These three are written by the watcher and VOD-transition goroutines while
	// HTTP handlers read them concurrently, so they are atomics rather than
	// plain fields — same reason as mode above.
	vodIndex          atomic.Pointer[index.ChannelIndex] // non-nil after a successful VOD transition
	finalDuration     atomic.Int64                       // time.Duration: mediaPresentationDuration from finalMPD
	staticIngestStart atomic.Pointer[time.Time]          // set when a static upstream is first detected

	// vodSuppressed blocks the automatic VOD transition for a channel that was
	// just restored to live. Without it the restore is futile: the upstream is
	// normally still finished, so the source would complete cleanly again
	// within seconds and put the channel straight back into VOD.
	//
	// storedBaseline is the stored-segment count at the moment of that restore.
	// The suppression lifts as soon as the count exceeds it, which is exactly
	// "the upstream produced something new" — Progress.AddStored only counts
	// segments the index accepted as new.
	vodSuppressed  atomic.Bool
	storedBaseline atomic.Uint64
}

// Config returns the channel's current configuration.
//
// It returns a copy by value: callers that read several fields should take one
// snapshot rather than calling this repeatedly, so a concurrent hot update
// cannot leave them mixing fields from two different configurations.
func (rt *Runtime) Config() Config {
	if c := rt.cfg.Load(); c != nil {
		return cloneConfig(*c)
	}
	return Config{}
}

// setConfig replaces the channel's configuration. It stores a copy so the
// caller cannot mutate what readers see afterwards.
func (rt *Runtime) setConfig(cfg Config) {
	stored := cloneConfig(cfg)
	rt.cfg.Store(&stored)
}

// Mode returns the current channel mode.
func (rt *Runtime) Mode() ChannelMode { return ChannelMode(rt.mode.Load()) }

// VODIndex returns the full-history index built during the VOD transition, or
// nil if the channel has not completed one.
func (rt *Runtime) VODIndex() *index.ChannelIndex { return rt.vodIndex.Load() }

// ActiveIndex returns the index that currently describes what the channel can
// serve: the full-history VOD index once a transition has completed, and the
// live index otherwise.
//
// It is a method rather than a rule each caller reapplies because getting it
// wrong is invisible — reading rt.Index directly on a transitioned channel
// silently serves the live window instead of the full asset. Note that
// ModeStaticIngesting deliberately stays on the live index: segments are still
// arriving, and the VOD index does not exist yet.
func (rt *Runtime) ActiveIndex() *index.ChannelIndex {
	if rt.Mode() == ModeVOD {
		if vod := rt.VODIndex(); vod != nil {
			return vod
		}
	}
	return rt.Index
}

// FinalDuration returns the mediaPresentationDuration announced by the upstream
// final MPD, or 0 when the upstream did not announce one.
func (rt *Runtime) FinalDuration() time.Duration {
	return time.Duration(rt.finalDuration.Load())
}

// StaticIngestStartTime returns when a static upstream was first detected, or
// the zero time if that has not happened.
func (rt *Runtime) StaticIngestStartTime() time.Time {
	if t := rt.staticIngestStart.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

func (rt *Runtime) setStaticIngestStart(t time.Time) { rt.staticIngestStart.Store(&t) }

// autoVODAllowed reports whether a clean source completion may transition this
// channel to VOD, lifting the suppression a restore installed once the upstream
// has produced a segment this run.
//
// The check is by counter rather than by a callback on the processor because
// AddStored is already the one place that means "a new segment was committed";
// a second notification path would be a second thing to keep true.
func (rt *Runtime) autoVODAllowed() bool {
	if !rt.vodSuppressed.Load() {
		return true
	}
	if rt.Progress.Snapshot().SegmentsStored > rt.storedBaseline.Load() {
		rt.vodSuppressed.Store(false)
		return true
	}
	return false
}

// IsHLSSource reports whether this channel ingests HLS.
//
// It reads the runtime that was actually built rather than the configuration
// that asked for it, which is what callers deciding "can I call rt.HLS" need.
func (rt *Runtime) IsHLSSource() bool { return rt.HLS != nil }

// ServesDASH reports whether the channel can produce a DASH manifest. An HLS
// source qualifies only when its segments are fMP4; MPEG-TS would need
// remuxing, which this gateway deliberately does not do.
func (rt *Runtime) ServesDASH() bool { return rt.HLS == nil || rt.HLS.IsFMP4() }

// CanServeKeys reports whether the channel has an AES-128 key cache to broker
// from. Whether it *should* is a separate policy question: decrypt-on-ingest
// channels store cleartext and must still be refused by the caller.
func (rt *Runtime) CanServeKeys() bool { return rt.HLS != nil && rt.Keys != nil }

// ParsedMPD returns the most recently fetched upstream MPD, or nil for HLS
// channels, which have no MPD at all.
func (rt *Runtime) ParsedMPD() *imdp.ParsedMPD {
	if rt.Watcher == nil {
		return nil
	}
	return rt.Watcher.ParsedMPD()
}

// ParsedMPDBytes returns the raw upstream MPD XML, or nil for HLS channels.
func (rt *Runtime) ParsedMPDBytes() []byte {
	if rt.Watcher == nil {
		return nil
	}
	return rt.Watcher.ParsedMPDBytes()
}

// Manager starts and stops channels at runtime.
type Manager struct {
	mu sync.RWMutex
	// operationMu serializes lifecycle writes. It is separate from mu so Get
	// remains available while Stop joins goroutines during an admin operation.
	operationMu sync.Mutex
	gwCfg       gocfg.Config // gateway-level config (data dir, worker counts, etc.)
	runtimes    map[string]*Runtime
	store       *Store            // optional; when set, VOD state is persisted on transition
	onVODReady  func(rt *Runtime) // optional; called after any successful VOD transition
	// suppressVOD carries the "do not auto-transition" mark across the restart
	// that RestoreToLive performs. It cannot live on the Runtime because that
	// restart builds a new one; Start consumes the entry and clears it.
	suppressVOD map[string]bool
}

// NewManager creates a Manager. gwCfg supplies gateway-level defaults.
func NewManager(gwCfg gocfg.Config) *Manager {
	return &Manager{
		gwCfg:       gwCfg,
		runtimes:    make(map[string]*Runtime),
		suppressVOD: make(map[string]bool),
	}
}

// SetStore attaches a channel store so the manager can persist VOD state
// transitions across restarts. Call before any Start().
func (m *Manager) SetStore(s *Store) {
	m.mu.Lock()
	m.store = s
	m.mu.Unlock()
}

// SetOnVODReady registers a callback invoked after any successful VOD transition
// (live→VOD or static-ingest-complete). Called in a separate goroutine.
//
// Must be set before any Start(): a channel that transitions before the callback
// is registered writes nothing, and the omission is silent until a restart
// serves an empty channel. Nothing enforces that ordering — the callback is a
// field on a live Manager, so there is no point at which it could be required —
// and no test covers it either. What is covered is the callback being absent
// altogether (cmd/gateway.TestNewGatewayRegistersVODSnapshotCallback).
//
// In practice the window is wide: starting channels only launches goroutines,
// so the restore loop finishes long before the first transition can complete.
// Registering late is wrong, but it does not currently lose anything.
func (m *Manager) SetOnVODReady(fn func(rt *Runtime)) {
	m.mu.Lock()
	m.onVODReady = fn
	m.mu.Unlock()
}

// Start launches all goroutines for a channel. Idempotent: if already running,
// the existing runtime is returned unchanged.
//
// The order here carries meaning that is easy to break: the ingest machinery is
// built before the runtime that publishes it, the runtime is registered before
// ingest starts, and a VOD channel returns before any ingest goroutine is
// launched at all. What each piece is made of lives in newIngestPipeline.
func (m *Manager) Start(parentCtx context.Context, id string, cfg Config) (*Runtime, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	return m.start(parentCtx, id, cfg)
}

// start is Start with operationMu already held.
func (m *Manager) start(parentCtx context.Context, id string, cfg Config) (*Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rt, ok := m.runtimes[id]; ok {
		return rt, nil // already running
	}

	lifecycleCtx, cancel := context.WithCancel(parentCtx)
	sourceCtx, sourceCancel := context.WithCancel(lifecycleCtx)
	poolCtx, poolCancel := context.WithCancel(lifecycleCtx)

	pl := newIngestPipeline(m.gwCfg, id, cfg)

	rt := &Runtime{
		ID:           id,
		Index:        pl.index,
		Watcher:      pl.watcher,
		HLS:          pl.hls,
		Keys:         pl.keys,
		Source:       pl.source,
		Progress:     pl.progress,
		Broker:       pl.broker,
		headers:      pl.headers,
		cancel:       cancel,
		lifecycleCtx: lifecycleCtx,
		sourceCancel: sourceCancel,
		poolCancel:   poolCancel,
		sourceDone:   make(chan struct{}),
		pipelineDone: make(chan struct{}),
		vodDone:      make(chan struct{}),
	}
	rt.setConfig(cfg)

	// Consume the mark left by RestoreToLive. The baseline is the same number
	// newIngestPipeline seeded into progress, so "stored has grown past it"
	// means a segment arrived during this run and not before it.
	if m.suppressVOD[id] {
		delete(m.suppressVOD, id)
		rt.vodSuppressed.Store(true)
		rt.storedBaseline.Store(storedSegments(pl.index))
	}

	if cfg.IsVOD {
		m.restoreVODState(rt, id)
	}

	m.runtimes[id] = rt

	// VOD channels are fully served from disk; no ingest goroutines needed.
	if cfg.IsVOD {
		slog.Info("channel started", "id", id)
		return rt, nil
	}

	m.launchIngest(sourceCtx, poolCtx, lifecycleCtx, rt, cfg, pl)

	slog.Info("channel started", "id", id)
	return rt, nil
}

// restoreVODState brings a channel back up in VOD mode from persisted state,
// without triggering a new transition.
func (m *Manager) restoreVODState(rt *Runtime, id string) {
	rt.vodIndex.Store(rt.Index)
	rt.mode.Store(int32(ModeVOD))
	rt.vodOnce.Do(func() { close(rt.vodDone) })

	// A restored VOD channel is complete by definition: nothing further will be
	// ingested, and what is on disk is the whole asset. Saying so lets the card
	// render "finished" rather than an empty progress bar. The original
	// discovery total is not on disk, so the restored count stands in for it.
	rt.Progress.SetExpectedTotal(rt.Progress.Snapshot().SegmentsStored)
	rt.Progress.Finalize()

	m.restoreVODUpstreamMPD(rt)

	slog.Info("channel restored in VOD mode", "id", id)
}

// launchIngest starts the channel's goroutines: the fetch pool, the processor,
// and the supervisor that keeps the source running.
//
// Every goroutine is counted in rt.wg, which is what lets Stop join them rather
// than merely cancel them — see Stop for why that matters.
func (m *Manager) launchIngest(sourceCtx, poolCtx, processorCtx context.Context, rt *Runtime, cfg Config, pl *ingestPipeline) {
	// pipelineWG lets finalization join just the pool and processor. It must not
	// wait on rt.wg: finalization itself is counted there, so that would make a
	// Stop racing a finalization deadlock.
	var pipelineWG sync.WaitGroup
	pipelineWG.Add(2)
	rt.wg.Add(5)
	go func() { defer rt.wg.Done(); defer pipelineWG.Done(); pl.pool.Run(poolCtx) }()
	go func() { defer rt.wg.Done(); defer pipelineWG.Done(); pl.processor.Run(processorCtx, pl.pool) }()
	go func() { defer rt.wg.Done(); m.superviseSource(sourceCtx, rt, cfg, pl) }()
	go func() { defer rt.wg.Done(); m.awaitStaticUpstream(sourceCtx, rt) }()
	go func() {
		defer rt.wg.Done()
		pipelineWG.Wait()
		close(rt.pipelineDone)
	}()
}

// staticUpstreamPollInterval is how often a running channel is asked whether
// its upstream turned out to be a finished asset.
const staticUpstreamPollInterval = time.Second

// awaitStaticUpstream moves a channel into ModeStaticIngesting as soon as the
// watcher has seen that the upstream is a finished asset.
//
// superviseSource also sets that mode, but only once the source returns, and
// the two used to be the same instant: enumeration was a tight push loop that
// returned in milliseconds. A paced producer does not return until every
// segment it named has been queued, which for an asset much larger than the
// queue is most of the way through the download — so the channel spent that
// whole time reporting itself as live, and the admin page showed LIVE for an
// asset that was plainly being fetched.
//
// This only promotes; the transition itself still waits for the source to
// return and the pipeline to drain, because that is what says the work is done.
// A channel whose auto-VOD is suppressed is left alone, matching the same
// exemption in superviseSource.
func (m *Manager) awaitStaticUpstream(ctx context.Context, rt *Runtime) {
	if rt.Watcher == nil && rt.HLS == nil {
		return
	}
	ticker := time.NewTicker(staticUpstreamPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !staticIngestVisible(rt.upstreamIsFinishedAsset(), rt.autoVODAllowed()) {
				continue
			}
			if rt.mode.CompareAndSwap(int32(ModeLive), int32(ModeStaticIngesting)) {
				rt.setStaticIngestStart(time.Now())
				slog.Info("upstream is a finished asset, ingesting", "channel", rt.ID)
			}
			return
		}
	}
}

// upstreamIsFinishedAsset reports whether this channel's source has said it is
// serving a completed asset rather than a live stream. Each protocol says it
// differently: DASH in the manifest's presentation type, HLS by every polled
// playlist carrying EXT-X-ENDLIST on its first poll.
func (rt *Runtime) upstreamIsFinishedAsset() bool {
	switch {
	case rt.Watcher != nil:
		return rt.Watcher.UpstreamType() == string(imdp.PresentationStatic)
	case rt.HLS != nil:
		return rt.HLS.UpstreamIsVOD()
	}
	return false
}

// staticIngestVisible reports whether a live channel should already be shown as
// ingesting a finished asset: the upstream has said so, and this channel is not
// one whose auto-VOD was deliberately suppressed.
func staticIngestVisible(upstreamFinished, autoVODAllowed bool) bool {
	return upstreamFinished && autoVODAllowed
}

// superviseSource runs the channel's source, restarting it with backoff until
// the context is cancelled or ingestion completes.
func (m *Manager) superviseSource(ctx context.Context, rt *Runtime, cfg Config, pl *ingestPipeline) {
	defer close(rt.sourceDone)
	backoff := 2 * time.Second
	for {
		err := rt.Source.Run(ctx)
		if err == nil {
			// A cancellation-aware source can race the manual transition and
			// return nil after its context has been cancelled. That is not an
			// end-of-stream signal, and in particular must not overwrite the
			// mode the manual path already claimed.
			if ctx.Err() != nil {
				return
			}
			// Source returned cleanly: ingestion is complete — a static MPD, or
			// an HLS playlist that reached EXT-X-ENDLIST. Enter StaticIngesting
			// mode and wait for the broker to drain before doing the full VOD
			// transition.
			//
			// Unless this channel was just restored to live, in which case the
			// upstream is expected to still be finished: fall through to the
			// backoff and keep re-running the source, so that a stream which
			// does come back is picked up (and lifts the suppression) instead
			// of the restore being undone within seconds.
			if !rt.autoVODAllowed() {
				slog.Info("source completed while auto-VOD is suppressed, waiting for upstream to resume",
					"channel", rt.ID, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 60*time.Second {
					backoff *= 2
				}
				continue
			}
			if !cfg.IsVOD && (rt.mode.CompareAndSwap(int32(ModeLive), int32(ModeStaticIngesting)) || rt.Mode() == ModeStaticIngesting) {
				if rt.StaticIngestStartTime().IsZero() {
					rt.setStaticIngestStart(time.Now())
				}
				rt.wg.Add(1)
				go func() {
					defer rt.wg.Done()
					m.awaitStaticIngestComplete(rt)
				}()
			}
			return
		}
		if err == context.Canceled {
			return
		}
		slog.Error("watcher stopped, restarting", "channel", rt.ID, "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// TransitionToVOD triggers a manual live→VOD transition for a channel.
// Returns an error if the channel is not running or already in VOD/transitioning mode.
func (m *Manager) TransitionToVOD(id string) error {
	// The whole check-and-launch runs under the read lock, because this is the
	// one place a goroutine is added to a channel from outside that channel's
	// own goroutines. Stop deletes the runtime under the write lock before it
	// waits, so holding the read lock here means either we still see the runtime
	// and Stop has not begun waiting, or it is already gone and we refuse —
	// never an Add racing a Wait.
	m.mu.RLock()
	defer m.mu.RUnlock()

	rt, ok := m.runtimes[id]
	if !ok {
		return fmt.Errorf("channel %q not running", id)
	}
	// Claim before launching the goroutine. Besides making the operation's
	// acceptance truthful, this is what makes a simultaneous clean source
	// completion and a manual request have exactly one winner.
	if !m.claimVODTransition(rt) {
		return fmt.Errorf("channel %q is not in live or static-ingesting mode", id)
	}
	// An explicit operator action overrides a restore's suppression.
	rt.vodSuppressed.Store(false)
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); m.finalizeVODTransition(rt) }()
	return nil
}

// drainCheckInterval is how often a channel whose source has finished is polled
// for the pipeline going idle. It can be short because Broker.Idle answers the
// question exactly: it used to be two seconds, which was not a poll interval so
// much as a grace period long enough for in-flight segments to land before an
// empty queue was mistaken for a finished ingest.
const drainCheckInterval = 100 * time.Millisecond

// awaitPipelineDrain waits for every task admitted before source shutdown to
// reach a terminal state, including any retries the processor requeues. An idle
// broker is stronger than an empty queue: it also excludes a fetch, result, or
// write still in flight.
func (m *Manager) awaitPipelineDrain(rt *Runtime, broker *queue.Broker) bool {
	if broker == nil {
		return !runtimeCancelled(rt)
	}
	ticker := time.NewTicker(drainCheckInterval)
	defer ticker.Stop()
	for {
		if broker.Idle() {
			return true
		}
		select {
		case <-runtimeDone(rt):
			return false
		case <-ticker.C:
		}
	}
}

// awaitStaticIngestComplete keeps a completed source visibly static-ingesting
// while the already admitted work drains. The final compare-and-swap is the
// automatic side of the manual/automatic race: a manual request can claim the
// transition during this wait and thereby becomes the only finalizer.
func (m *Manager) awaitStaticIngestComplete(rt *Runtime) {
	if !m.awaitPipelineDrain(rt, rt.Broker) || !m.claimVODTransition(rt) {
		return
	}
	m.finalizeVODTransition(rt)
}

func runtimeDone(rt *Runtime) <-chan struct{} {
	if rt.lifecycleCtx == nil {
		return nil
	}
	return rt.lifecycleCtx.Done()
}

func runtimeCancelled(rt *Runtime) bool {
	select {
	case <-runtimeDone(rt):
		return true
	default:
		return false
	}
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

// claimVODTransition changes the visible lifecycle state before any waiting.
// It is deliberately a separate operation because a manual request must be
// rejected synchronously when automatic completion won the same race.
func (m *Manager) claimVODTransition(rt *Runtime) bool {
	if rt.mode.CompareAndSwap(int32(ModeLive), int32(ModeTransitioning)) ||
		rt.mode.CompareAndSwap(int32(ModeStaticIngesting), int32(ModeTransitioning)) {
		slog.Info("channel transitioning to VOD", "id", rt.ID)
		return true
	}
	return false
}

// finalizeVODTransition turns a claimed transition into VOD. It first closes
// source admission, then drains and joins the pipeline, so the disk rebuild
// sees every task that was already accepted when the transition was requested.
func (m *Manager) finalizeVODTransition(rt *Runtime) {
	if rt.sourceCancel != nil {
		rt.sourceCancel()
	}
	if rt.sourceDone != nil {
		select {
		case <-rt.sourceDone:
		case <-runtimeDone(rt):
			return
		}
	}
	// The last successful source poll may have updated the parsed MPD just
	// before Source.Run returned. Capture it only after source exit so the final
	// duration reflects that last observation.
	finalMPD := rt.ParsedMPD()
	if runtimeCancelled(rt) || !m.awaitPipelineDrain(rt, rt.Broker) {
		return
	}
	logStaticIngestShortfall(rt)

	// No work remains. Stop the pool so it closes Results. The processor then
	// exits naturally after its final result and waits for key warming under the
	// still-live runtime context; cancelling it here would abandon a
	// passthrough key cache just before the VOD needs to serve it offline.
	// Waiting only for pipelineDone avoids waiting for this finalizer through
	// rt.wg itself.
	if rt.poolCancel != nil {
		rt.poolCancel()
	}
	if rt.pipelineDone != nil {
		select {
		case <-rt.pipelineDone:
		case <-runtimeDone(rt):
			return
		}
	}
	if runtimeCancelled(rt) {
		return
	}
	m.publishVODTransition(rt, finalMPD)
}

// publishVODTransition rebuilds a full-history index from disk and publishes
// it after its caller has made the ingest pipeline quiescent.
func (m *Manager) publishVODTransition(rt *Runtime, finalMPD *imdp.ParsedMPD) {
	if finalMPD != nil && finalMPD.MediaPresentationDuration > 0 {
		rt.finalDuration.Store(int64(finalMPD.MediaPresentationDuration))
	}

	// Rebuild from disk with unbounded ring buffers so the VOD covers the whole
	// recorded history rather than just what the live ring still held. fMP4
	// timing comes from each segment's tfdt box and MPEG-TS timing from the
	// per-representation sidecar, so both formats reconstruct fully.
	vodCI := index.NewChannelIndex()
	if err := index.RebuildFromDiskForVOD(vodCI, m.gwCfg.Store.DataDir, rt.ID); err != nil {
		slog.Error("VOD rebuild failed", "channel", rt.ID, "err", err)
		// Source admission and the pipeline have already stopped. Reporting live
		// here would promise an ingest that no longer exists, so leave the
		// runtime transitioning for an operator to restart or inspect.
		return
	}

	rt.vodOnce.Do(func() {
		rt.vodIndex.Store(vodCI)
		rt.mode.Store(int32(ModeVOD))
		close(rt.vodDone)
		slog.Info("channel VOD ready", "id", rt.ID)

		// Ingest is over. The cumulative stored count is deliberately left alone —
		// after a live→VOD transition, how much the channel actually recorded is
		// precisely what an operator wants to see. Only a channel that never had a
		// discovery total (a live one) adopts its stored count as the total, so a
		// static ingest that lost segments still reports 500/512 rather than
		// quietly rewriting itself as 500/500.
		snap := rt.Progress.Snapshot()
		if !snap.TotalKnown {
			rt.Progress.SetExpectedTotal(snap.SegmentsStored)
		}
		rt.Progress.Finalize()

		// Persist VOD state so the gateway restores VOD mode after a restart.
		m.mu.RLock()
		st := m.store
		cb := m.onVODReady
		m.mu.RUnlock()
		if st != nil {
			// Merge under the store's lock rather than Get+Put: an operator can
			// be saving an edit to this same channel right now, and a plain
			// read-modify-write would silently drop whichever side lost.
			rt.configMu.Lock()
			merged, err := st.Update(rt.ID, func(c *Config) { c.IsVOD = true })
			if err != nil {
				slog.Warn("failed to persist VOD state", "channel", rt.ID, "err", err)
			} else {
				// Adopt the merged result so the runtime reflects both this
				// transition and any concurrent edit. Safe now that Config is
				// behind an atomic; it used to be left alone because HTTP
				// handlers read the field without synchronisation.
				rt.setConfig(merged)
			}
			rt.configMu.Unlock()
		}
		if cb != nil {
			// Counted like every other channel goroutine: the callback snapshots
			// the final manifest into this channel's own data directory, so Stop
			// must not return while it is still writing there. Adding here is
			// safe because every path into publishVODTransition already runs inside a
			// counted goroutine.
			rt.wg.Add(1)
			go func() { defer rt.wg.Done(); cb(rt) }()
		}
	})
}

// Stop shuts down the channel's goroutines and does not return until they have
// all exited.
//
// Joining rather than merely cancelling is what makes an admin restart safe: the
// replacement channel writes to the same segment files and the same
// segments.jsonl, so a predecessor still inside the processor would interleave
// with it — and a sidecar compaction on either side would drop the other's
// records.
//
// The lock is released before the wait, deliberately. Every HTTP handler reaches
// its channel through Get, which takes m.mu for reading; waiting under the write
// lock would stall the entire gateway for as long as one channel takes to drain.
// The runtime is removed from the map first, so nothing new can find it while it
// is winding down.
func (m *Manager) Stop(id string) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	return m.stop(id)
}

// stop is Stop with operationMu already held.
func (m *Manager) stop(id string) error {
	m.mu.Lock()
	rt, ok := m.runtimes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("channel %q not running", id)
	}
	delete(m.runtimes, id)
	m.mu.Unlock()

	rt.cancel()
	rt.wg.Wait()
	slog.Info("channel stopped", "id", id)
	return nil
}

// Get returns the runtime for a channel, or nil if not running.
func (m *Manager) Get(id string) *Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runtimes[id]
}

// StopAll stops every running channel and waits for all of them. Used on
// gateway shutdown.
//
// Same lock discipline as Stop: detach the runtimes under the lock, release it,
// then cancel and wait. Cancelling every channel before waiting on any of them
// makes shutdown take as long as the slowest channel rather than their sum.
func (m *Manager) StopAll() {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	m.mu.Lock()
	stopping := make(map[string]*Runtime, len(m.runtimes))
	for id, rt := range m.runtimes {
		stopping[id] = rt
		delete(m.runtimes, id)
	}
	m.mu.Unlock()

	for _, rt := range stopping {
		rt.cancel()
	}
	for id, rt := range stopping {
		rt.wg.Wait()
		slog.Info("channel stopped", "id", id)
	}
}

// Status returns a point-in-time snapshot of all running channels.
func (m *Manager) Status() map[string]ChannelStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]ChannelStatus, len(m.runtimes))
	for id, rt := range m.runtimes {
		out[id] = buildStatus(rt)
	}
	return out
}

// StatusFor returns a snapshot of one running channel, or false if it is not
// running. It exists so a single-channel request does not have to build every
// other channel's status — including their per-track breakdowns — to throw all
// but one away.
func (m *Manager) StatusFor(id string) (ChannelStatus, bool) {
	m.mu.RLock()
	rt, ok := m.runtimes[id]
	m.mu.RUnlock()
	if !ok {
		return ChannelStatus{}, false
	}
	return buildStatus(rt), true
}

// ChannelStatus is a JSON-serialisable snapshot of a running channel.
type ChannelStatus struct {
	ID      string        `json:"id"`
	MPDURL  string        `json:"mpd_url"`
	Running bool          `json:"running"`
	Mode    string        `json:"mode"`
	Periods []PeriodStats `json:"periods"`
	// Progress is what the channel has ingested since it was last started.
	// Absent for a channel that is not running.
	Progress *ProgressStats `json:"progress,omitempty"`
	// Tracks is the per-representation breakdown. Absent until the first segment
	// is indexed.
	Tracks []TrackStats `json:"tracks,omitempty"`
	// Source reports how close the upstream is to being finished. A pointer so
	// a status with nothing to say serialises to the same keys it always had.
	Source *SourceStatus `json:"source_status,omitempty"`
}

// SourceStatus reports the upstream's end-of-stream signal, which is what
// decides when a channel transitions to VOD: the MPD's presentation type for
// DASH, and the EXT-X-ENDLIST count across poll targets for HLS.
type SourceStatus struct {
	UpstreamType string `json:"upstream_type,omitempty"` // DASH: "dynamic" | "static"
	EndedTargets int    `json:"ended_targets,omitempty"` // HLS: playlists that reached ENDLIST
	PollTargets  int    `json:"poll_targets,omitempty"`  // HLS: playlists being polled
}

// PeriodStats summarises one period's segment counts.
type PeriodStats struct {
	PeriodID  string `json:"period_id"`
	Published int    `json:"published"`
	Committed int    `json:"committed"`
	Expired   int    `json:"expired"`
	// Total is the number of segments the index currently holds, which is always
	// exactly Published+Committed+Expired — the ring buffer it measures only grows
	// on commit. It is therefore useless as a download denominator, despite the
	// name: use ProgressStats.TotalSegments for that. Kept for compatibility.
	Total int `json:"total"`
}

// storedSegments counts the segments an index already tracks, in any status.
//
// This is the sum PeriodStats.Total reports, and the only correct use for it: as
// the count of what is on disk right now. It was never a meaningful denominator
// for a download bar, because the ring buffer it measures only grows when a
// segment is committed — so it always equalled the number already stored.
func storedSegments(ci *index.ChannelIndex) uint64 {
	var n uint64
	ci.ForEachRep(func(_ index.RepRef, rep *index.RepresentationState) {
		p, c, e := rep.StatusCounts()
		n += uint64(p + c + e)
	})
	return n
}

// sourceStatus reports what the channel's watcher knows about the upstream
// ending, or nil when it knows nothing yet — before the first MPD fetch, or
// before HLS discovery has named the playlists to poll.
func sourceStatus(rt *Runtime) *SourceStatus {
	switch {
	case rt.Watcher != nil:
		if t := rt.Watcher.UpstreamType(); t != "" {
			return &SourceStatus{UpstreamType: t}
		}
	case rt.HLS != nil:
		if ended, total := rt.HLS.EndListProgress(); total > 0 {
			return &SourceStatus{EndedTargets: ended, PollTargets: total}
		}
	}
	return nil
}

func buildStatus(rt *Runtime) ChannelStatus {
	// One read for the whole status: the reported mode and the index the counts
	// come from must describe the same instant, or an operator sees "live" over
	// full-history numbers during a transition.
	sv := rt.Serving()

	modeStr := "live"
	switch sv.Mode {
	case ModeTransitioning:
		modeStr = "transitioning"
	case ModeVOD:
		modeStr = "vod"
	case ModeStaticIngesting:
		modeStr = "static_ingesting"
	}
	s := ChannelStatus{
		ID:      rt.ID,
		MPDURL:  sv.Config.MPDURL,
		Running: true,
		Mode:    modeStr,
		Source:  sourceStatus(rt),
	}
	ci := sv.Index
	stats := make(map[string]*PeriodStats)
	ci.ForEachRep(func(ref index.RepRef, rep *index.RepresentationState) {
		ps, ok := stats[ref.PeriodID]
		if !ok {
			ps = &PeriodStats{PeriodID: ref.PeriodID}
			stats[ref.PeriodID] = ps
		}
		p, c, e := rep.StatusCounts()
		ps.Published += p
		ps.Committed += c
		ps.Expired += e
		ps.Total += rep.Total()
	})
	// A period can exist with no AdaptationSets yet — serving a manifest creates
	// period state before any segment commits — and the hand-rolled walk this
	// replaced reported those with zero counts. Keep doing that.
	ci.Mu().RLock()
	for _, pid := range ci.PeriodIDs() {
		if _, ok := stats[pid]; !ok {
			stats[pid] = &PeriodStats{PeriodID: pid}
		}
	}
	ci.Mu().RUnlock()

	for _, ps := range stats {
		s.Periods = append(s.Periods, *ps)
	}

	s.Progress = buildProgress(rt)
	s.Tracks = buildTracks(rt, ci)
	return s
}
