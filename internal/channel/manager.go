package channel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/dashingest"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/pipeline"
	"github.com/a840817a/sluice/internal/presentation"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

// Runtime holds the live state for one running channel.
type Runtime struct {
	ID    string
	Index *index.ChannelIndex
	// watcher is the DASH ingest watcher; nil for HLS channels.
	watcher *dashingest.Watcher
	// hls is the HLS ingest watcher; nil for DASH channels.
	hls *hlsingest.Watcher
	// decrypt is the registry of a DASH channel in key mode decrypt, nil for
	// every other channel. ParsedMPD reads it through decryptView.
	decrypt     *dashdecrypt.Registry
	decryptView atomic.Pointer[decryptedView]
	// processor stores the channel's media while it ingests; nil for a
	// channel started in VOD, which ingests nothing.
	processor *pipeline.Processor
	// mediaMu is held by whoever rewrites stored media while the processor
	// is not running: the VOD transition from its rebuild to its exports,
	// and the final phase of a conversion (exclusiveMedia).
	mediaMu sync.Mutex
	// keys caches AES-128 content keys for HLS channels; nil for DASH channels.
	// Other packages reach what these three know through Source, Serving's
	// Tracks and ResolveKey, never directly.
	keys *hlskey.Cache
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
	// it here is what lets UpdateConfig rotate origin credentials in place.
	headers *fetch.AtomicHeaders
	// cancel ends the whole runtime. Source and pipeline have child contexts,
	// reached through ingest, so the VOD transition can stop admitting work
	// while allowing work already admitted to finish.
	cancel       context.CancelFunc
	lifecycleCtx context.Context
	ingest       ingestControl
	// wg counts every goroutine started on this channel's behalf, so Stop can
	// join them instead of merely cancelling. Each Add happens either before
	// Start returns or inside a goroutine that is itself already counted, so it
	// can never race the Wait in Stop.
	wg sync.WaitGroup

	// cfg is the channel's configuration. It is behind an atomic because the
	// admin API swaps it while HTTP handlers read it — see Config below.
	// configMu coordinates the store commit and runtime publication with the
	// VOD transition. It is deliberately never held while stopping a runtime:
	// Stop waits for the goroutines that can need this mutex.
	configMu sync.Mutex
	cfg      atomic.Pointer[Config]
	// modes owns the Channel mode and the auto-VOD suppression; every change
	// to either goes through it.
	modes   channelModeMachine
	vodOnce sync.Once
	vodDone chan struct{} // closed when VOD index is ready
	// vodWarnings lists the restart-safety steps that failed during the last
	// VOD transition. They do not block completion, which is served from the
	// in-memory index regardless; they tell an operator what a restart may lose.
	vodWarnings atomic.Pointer[[]string]

	// These three are written by the watcher and VOD-transition goroutines while
	// HTTP handlers read them concurrently, so they are atomics rather than
	// plain fields.
	vodIndex          atomic.Pointer[index.ChannelIndex] // non-nil after a successful VOD transition
	vodMPD            atomic.Pointer[imdp.ParsedMPD]     // DASH: what ParsedMPD returns once VOD (vodOriginMPD)
	savedDASH         atomic.Pointer[[]byte]             // DASH: saved manifest of a VOD restored without its origin MPD
	finalDuration     atomic.Int64                       // time.Duration: mediaPresentationDuration from finalMPD
	staticIngestStart atomic.Pointer[time.Time]          // set when a static origin is first detected
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
func (rt *Runtime) Mode() ChannelMode { return rt.modes.Mode() }

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

// FinalDuration returns the mediaPresentationDuration announced by the origin
// final MPD, or 0 when the origin did not announce one.
func (rt *Runtime) FinalDuration() time.Duration {
	return time.Duration(rt.finalDuration.Load())
}

// StaticIngestStartTime returns when a static origin was first detected, or
// the zero time if that has not happened.
func (rt *Runtime) StaticIngestStartTime() time.Time {
	if t := rt.staticIngestStart.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

func (rt *Runtime) setStaticIngestStart(t time.Time) { rt.staticIngestStart.Store(&t) }

// autoVODAllowed reports whether a clean source completion may transition this
// channel to VOD, lifting the suppression a restore installed once the origin
// has produced a segment this run. Progress.AddStored only counts segments the
// index accepted as new, so its total growing is exactly "something new".
func (rt *Runtime) autoVODAllowed() bool {
	return rt.modes.AutoAllowed(rt.Progress.Snapshot().SegmentsStored)
}

// IsHLSSource reports whether this channel ingests HLS.
//
// It reads the runtime that was actually built rather than the configuration
// that asked for it.
func (rt *Runtime) IsHLSSource() bool { return rt.hls != nil }

// ParsedMPD returns the most recently fetched origin MPD, or nil for HLS
// channels, which have no MPD at all. Once the channel is VOD it is that MPD
// with every period the VOD index holds put back (vodOriginMPD).
//
// On a DASH decrypt channel the AdaptationSets it decrypted carry no
// ContentProtection (withoutDecryptedCP). Every manifest, playlist and track
// list is built from what this returns, so none of them signals protection
// for content stored clear.
func (rt *Runtime) ParsedMPD() *imdp.ParsedMPD {
	if p := rt.vodMPD.Load(); p != nil {
		return rt.withoutDecryptedCP(p)
	}
	if rt.watcher == nil {
		return nil
	}
	return rt.withoutDecryptedCP(rt.watcher.ParsedMPD())
}

// ParsedMPDBytes returns the raw origin MPD XML, or nil for HLS channels. It
// still carries every ContentProtection: it is saved for a VOD restart to
// parse again, and never served.
func (rt *Runtime) ParsedMPDBytes() []byte {
	if rt.watcher == nil {
		return nil
	}
	return rt.watcher.ParsedMPDBytes()
}

// Manager starts and stops channels at runtime.
type Manager struct {
	mu sync.RWMutex
	// operationGate admits ordinary single-channel lifecycle operations. StopAll
	// takes it exclusively so shutdown neither misses a start in progress nor
	// permits a new one while it is draining the runtimes it detached.
	operationGate sync.RWMutex
	// operationLocks serializes lifecycle operations for one channel without
	// making an unrelated channel wait for a slow Stop callback. Entries are
	// removed after their last holder or waiter leaves.
	operationLocksMu sync.Mutex
	operationLocks   map[string]*channelOperationLock
	gwCfg            gocfg.Config // gateway-level config (data dir, worker counts, etc.)
	presentations    *presentation.Generator
	// vod owns the files a VOD transition writes and a VOD restore reads.
	vod      vodAsset
	runtimes map[string]*Runtime
	// store is where every channel's configuration lives. The manager is the
	// only writer, and a channel runs only while it is in the store.
	store      *Store
	onVODReady func(rt *Runtime) // optional; called after any successful VOD transition
	// suppressVOD carries the "do not auto-transition" mark across the restart
	// that RestoreToLive performs. It cannot live on the Runtime because that
	// restart builds a new one; start consumes the entry and clears it.
	suppressVOD map[string]bool
}

// NewManager creates a Manager over the channel configurations in store. gwCfg
// supplies gateway-level defaults.
func NewManager(gwCfg gocfg.Config, store *Store) *Manager {
	presentations := presentation.NewGenerator(gwCfg)
	return &Manager{
		gwCfg:          gwCfg,
		presentations:  presentations,
		vod:            vodAsset{dataDir: gwCfg.Store.DataDir, render: presentations},
		store:          store,
		runtimes:       make(map[string]*Runtime),
		suppressVOD:    make(map[string]bool),
		operationLocks: make(map[string]*channelOperationLock),
	}
}

// SetOnVODReady installs an optional completion observer. Required artifacts
// and VOD configuration are persisted by the manager regardless of this hook.
// The observer runs after optional exports and is joined by Stop.
func (m *Manager) SetOnVODReady(fn func(rt *Runtime)) {
	m.mu.Lock()
	m.onVODReady = fn
	m.mu.Unlock()
}

// StartEnabled starts every enabled channel in the store. It is how the
// gateway brings its channels up at boot. A channel that fails to start, or
// whose stored ID is not a valid channel ID — the store file can be edited by
// hand — is logged and skipped, so one bad channel does not keep the others
// down.
func (m *Manager) StartEnabled(ctx context.Context) {
	for id, cfg := range m.store.List() {
		if !cfg.Enabled {
			continue
		}
		if !ValidChannelID(id) {
			slog.Error("channel not started: invalid id in the channel store", "id", id)
			continue
		}
		unlock := m.lockOperation(id)
		_, err := m.start(ctx, id, cfg)
		unlock()
		if err != nil {
			slog.Error("channel start failed", "id", id, "err", err)
		}
	}
}

// start launches all goroutines for a channel. Idempotent: if already running,
// the existing runtime is returned unchanged. id's operation lock must be held,
// and cfg must be the channel's stored configuration: every entry point that
// calls it (Create, StartEnabled, the restarts) persists first.
//
// The order here carries meaning that is easy to break: the ingest machinery is
// built before the runtime that publishes it, the runtime is registered before
// ingest starts, and a VOD channel returns before any ingest goroutine is
// launched at all. What each piece is made of lives in newIngestPipeline.
func (m *Manager) start(parentCtx context.Context, id string, cfg Config) (*Runtime, error) {
	m.mu.RLock()
	if rt, ok := m.runtimes[id]; ok {
		m.mu.RUnlock()
		return rt, nil // already running
	}
	m.mu.RUnlock()

	// A conversion a crash interrupted is finished or undone before the
	// index is rebuilt, so it sees whole AdaptationSet directories. One that
	// cannot be keeps the channel down: ingest would write into a directory
	// whose other copy recovery has not dared to remove.
	if err := dashdecrypt.RecoverStaging(store.ChannelDir(m.gwCfg.Store.DataDir, id)); err != nil {
		return nil, fmt.Errorf("recover interrupted decryption: %w", err)
	}

	// Consume the mark left by RestoreToLive. Without it the restore is futile:
	// the origin is normally still finished, so the source would complete
	// cleanly again within seconds and put the channel straight back into VOD.
	m.mu.Lock()
	suppress := m.suppressVOD[id]
	delete(m.suppressVOD, id)
	m.mu.Unlock()

	lifecycleCtx, cancel := context.WithCancel(parentCtx)
	sourceCtx, sourceCancel := context.WithCancel(lifecycleCtx)
	poolCtx, poolCancel := context.WithCancel(lifecycleCtx)

	pl := newIngestPipeline(m.gwCfg, id, cfg)
	var parsedMPD func() *imdp.ParsedMPD
	if pl.watcher != nil {
		parsedMPD = pl.watcher.ParsedMPD
	} else {
		parsedMPD = func() *imdp.ParsedMPD { return nil }
	}
	ingest := newChannelIngest(sourceCancel, poolCancel, pl.broker, parsedMPD)

	rt := &Runtime{
		ID:           id,
		Index:        pl.index,
		watcher:      pl.watcher,
		hls:          pl.hls,
		keys:         pl.keys,
		decrypt:      pl.decrypt,
		Source:       pl.source,
		Progress:     pl.progress,
		Broker:       pl.broker,
		headers:      pl.headers,
		cancel:       cancel,
		lifecycleCtx: lifecycleCtx,
		ingest:       ingest,
		vodDone:      make(chan struct{}),
	}
	rt.setConfig(cfg)
	// The baseline is the same number newIngestPipeline seeded into progress,
	// so "stored has grown past it" means a segment arrived during this run and
	// not before it.
	initial := ModeLive
	if cfg.IsVOD {
		initial = ModeVOD
	}
	rt.modes.begin(initial, suppress, storedSegments(pl.index))
	if cfg.IsVOD {
		m.restoreVODState(rt, id)
	}

	if !cfg.IsVOD {
		rt.processor = pl.processor
	}

	m.mu.Lock()
	m.runtimes[id] = rt
	m.launchConversions(lifecycleCtx, rt)

	// VOD channels are fully served from disk; no ingest goroutines needed.
	if cfg.IsVOD {
		m.mu.Unlock()
		slog.Info("channel started", "id", id)
		return rt, nil
	}

	// Count and launch while the runtime is published under m.mu. Stop removes
	// it under the same lock before calling Wait, so it cannot observe a zero
	// WaitGroup between publication and this Add.
	m.launchIngest(sourceCtx, poolCtx, lifecycleCtx, rt, cfg, pl, ingest)
	m.mu.Unlock()

	slog.Info("channel started", "id", id)
	return rt, nil
}

// restoreVODState brings a channel back up in VOD mode from persisted state,
// without triggering a new transition. The runtime already starts in ModeVOD.
func (m *Manager) restoreVODState(rt *Runtime, id string) {
	// A restored VOD channel is complete by definition: nothing further will be
	// ingested, and what is on disk is the whole asset. Saying so lets the card
	// render "finished" rather than an empty progress bar.
	rt.enterVOD(rt.Index, false)

	m.vod.restore(rt)

	slog.Info("channel restored in VOD mode", "id", id)
}

// TransitionToVOD triggers a manual live→VOD transition for a channel.
// A failed rebuild can be retried without restarting ingest. Returns an error
// if the channel is not running or already in VOD/transitioning mode.
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
	// completion and a manual request have exactly one winner. A manual claim
	// also retries a failed transition and overrides a restore's suppression.
	if from, ok := rt.modes.Claim(claimManual); !ok {
		return fmt.Errorf("channel %q is not in live or static-ingesting mode (it is %s)", id, from)
	}
	slog.Info("channel transitioning to VOD", "id", rt.ID, "trigger", "manual")
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); newVODTransition(m, rt).Run(lifecycleContext(rt)) }()
	return nil
}

// awaitStaticIngestComplete keeps a completed source visibly static-ingesting
// while the already admitted work drains. The claim is the automatic side of
// the manual/automatic race: a manual request can claim the
// transition during this wait and thereby becomes the only one to run it.
func (m *Manager) awaitStaticIngestComplete(rt *Runtime) {
	ctx := lifecycleContext(rt)
	if !rt.ingest.AwaitDrain(ctx) {
		return
	}
	if _, ok := rt.modes.Claim(claimAuto); !ok {
		return
	}
	slog.Info("channel transitioning to VOD", "id", rt.ID, "trigger", "auto")
	newVODTransition(m, rt).Run(ctx)
}

// lifecycleContext is the runtime's lifecycle context, or Background for a
// runtime built without one.
func lifecycleContext(rt *Runtime) context.Context {
	if rt.lifecycleCtx == nil {
		return context.Background()
	}
	return rt.lifecycleCtx
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
	defer m.lockOperation(id)()
	return m.stop(id)
}

// stop is Stop with id's operation lock already held.
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
	m.operationGate.Lock()
	defer m.operationGate.Unlock()
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
