package channel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	"github.com/a840817a/sluice/internal/pipeline"
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
	// it here is what lets ApplyConfig rotate upstream credentials in place.
	headers *fetch.AtomicHeaders
	cancel  context.CancelFunc
	// wg counts every goroutine started on this channel's behalf, so Stop can
	// join them instead of merely cancelling. Each Add happens either before
	// Start returns or inside a goroutine that is itself already counted, so it
	// can never race the Wait in Stop.
	wg sync.WaitGroup

	// cfg is the channel's configuration. It is behind an atomic because the
	// admin API swaps it while HTTP handlers read it — see Config below.
	cfg     atomic.Pointer[Config]
	mode    atomic.Int32 // ChannelMode
	vodOnce sync.Once
	vodDone chan struct{} // closed when VOD index is ready

	// These three are written by the watcher and VOD-transition goroutines while
	// HTTP handlers read them concurrently, so they are atomics rather than
	// plain fields — same reason as mode above.
	vodIndex          atomic.Pointer[index.ChannelIndex] // non-nil after a successful VOD transition
	finalDuration     atomic.Int64                       // time.Duration: mediaPresentationDuration from finalMPD
	staticIngestStart atomic.Pointer[time.Time]          // set when a static upstream is first detected
}

// Config returns the channel's current configuration.
//
// It returns a copy by value: callers that read several fields should take one
// snapshot rather than calling this repeatedly, so a concurrent hot update
// cannot leave them mixing fields from two different configurations.
func (rt *Runtime) Config() Config {
	if c := rt.cfg.Load(); c != nil {
		return *c
	}
	return Config{}
}

// setConfig replaces the channel's configuration. It stores a copy so the
// caller cannot mutate what readers see afterwards.
func (rt *Runtime) setConfig(cfg Config) { rt.cfg.Store(&cfg) }

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
	mu         sync.RWMutex
	gwCfg      gocfg.Config // gateway-level config (data dir, worker counts, etc.)
	runtimes   map[string]*Runtime
	store      *Store            // optional; when set, VOD state is persisted on transition
	onVODReady func(rt *Runtime) // optional; called after any successful VOD transition
}

// NewManager creates a Manager. gwCfg supplies gateway-level defaults.
func NewManager(gwCfg gocfg.Config) *Manager {
	return &Manager{
		gwCfg:    gwCfg,
		runtimes: make(map[string]*Runtime),
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
// Must be set before any Start().
func (m *Manager) SetOnVODReady(fn func(rt *Runtime)) {
	m.mu.Lock()
	m.onVODReady = fn
	m.mu.Unlock()
}

// ingestPipeline is the per-channel machinery both protocols share: one queue,
// one worker pool, one processor, and the counters they feed. It is passed
// between Start's phases so each of them can stay a named step rather than
// another hundred lines of the same function.
type ingestPipeline struct {
	headers   *fetch.AtomicHeaders
	broker    *queue.Broker
	pool      *fetch.Pool
	processor *pipeline.Processor
	progress  *progress.Counters
}

// Start launches all goroutines for a channel. Idempotent: if already running,
// the existing runtime is returned unchanged.
//
// The body is a sequence of named phases rather than one long composition,
// because the order between them carries meaning that is easy to break: the
// index has to be restored before the processor is built (they must agree on
// ring capacity), the runtime has to be registered before ingest starts, and a
// VOD channel must return before any ingest goroutine is launched at all.
func (m *Manager) Start(parentCtx context.Context, id string, cfg Config) (*Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rt, ok := m.runtimes[id]; ok {
		return rt, nil // already running
	}

	ctx, cancel := context.WithCancel(parentCtx)

	chGwCfg := m.channelGatewayConfig(cfg)
	ci := m.restoreIndex(id, cfg)
	pl := m.newIngestPipeline(id, cfg, chGwCfg, ci)

	rt := &Runtime{
		ID:       id,
		Index:    ci,
		Progress: pl.progress,
		Broker:   pl.broker,
		headers:  pl.headers,
		cancel:   cancel,
		vodDone:  make(chan struct{}),
	}
	rt.setConfig(cfg)

	m.attachSource(rt, id, cfg, chGwCfg, pl)

	if cfg.IsVOD {
		m.restoreVODState(rt, id)
	}

	m.runtimes[id] = rt

	// VOD channels are fully served from disk; no ingest goroutines needed.
	if cfg.IsVOD {
		slog.Info("channel started", "id", id)
		return rt, nil
	}

	m.launchIngest(ctx, rt, cfg, pl)

	slog.Info("channel started", "id", id)
	return rt, nil
}

// channelGatewayConfig merges gateway defaults with this channel's settings.
//
// The result is a copy: several fields are per-channel only (marked yaml:"-" on
// the config type) and exist so downstream constructors can read one config
// instead of a config plus a scattering of overrides.
func (m *Manager) channelGatewayConfig(cfg Config) gocfg.Config {
	chGwCfg := m.gwCfg
	chGwCfg.Upstream.MPDURL = cfg.MPDURL
	chGwCfg.Upstream.ForwardClientIP = cfg.ForwardClientIP
	chGwCfg.Store.EnableVODTransition = cfg.EnableVODTransition
	chGwCfg.Store.KeepAllSegments = cfg.KeepAllSegments
	return chGwCfg
}

// restoreIndex rebuilds the channel's segment index from what is already on
// disk, so a restart resumes instead of re-fetching.
//
// A rebuild failure is logged and not returned: an empty index means the
// channel re-ingests, which is recoverable, whereas refusing to start leaves it
// down until someone intervenes.
func (m *Manager) restoreIndex(id string, cfg Config) *index.ChannelIndex {
	ci := index.NewChannelIndex()
	if cfg.IsVOD {
		// A channel already transitioned to VOD keeps its full history, so the
		// rebuild uses unbounded rings.
		if err := index.RebuildFromDiskForVOD(ci, m.gwCfg.Store.DataDir, id); err != nil {
			slog.Warn("VOD disk rebuild failed", "channel", id, "err", err)
		}
		return ci
	}
	// The rebuilt index must use the same ring capacity the processor will, so
	// the rule lives in one place.
	capacity := pipeline.SegmentCapacity(cfg.KeepAllSegments, cfg.EnableVODTransition)
	if err := index.RebuildFromDiskWithCapacity(ci, m.gwCfg.Store.DataDir, id, capacity); err != nil {
		slog.Warn("disk rebuild failed", "channel", id, "err", err)
	}
	return ci
}

// newIngestPipeline builds the queue, worker pool and processor for a channel.
func (m *Manager) newIngestPipeline(id string, cfg Config, chGwCfg gocfg.Config, ci *index.ChannelIndex) *ingestPipeline {
	// One provider shared by the fetcher, the watcher and the key cache, so a
	// credential rotation applied via ApplyConfig reaches all three at once.
	headers := fetch.NewAtomicHeaders(HeaderMap(cfg.FetchHeaders))

	broker := queue.NewBroker(4096)
	fetcher := fetch.NewFetcher(nil, headers, m.gwCfg.Worker.MaxSegmentBytes, m.gwCfg.Worker.SegmentTimeout)
	pool := fetch.NewPool(fetcher, broker, m.gwCfg.Worker.FetchWorkers)
	processor := pipeline.NewProcessor(chGwCfg, id, ci, broker)

	// Progress counts from this start onwards, so the segments the rebuild
	// restored from disk are seeded in rather than re-counted as fresh work.
	// They are seeded without throughput samples: they were not fetched during
	// this run, and treating them as such would render an absurd rate and ETA.
	prog := progress.New()
	prog.SeedStored(storedSegments(ci))
	processor.SetProgress(prog)

	return &ingestPipeline{
		headers:   headers,
		broker:    broker,
		pool:      pool,
		processor: processor,
		progress:  prog,
	}
}

// attachSource picks the ingest driver for this channel's upstream protocol.
// Both feed the same broker, processor, store and index; only discovery and the
// publish rule differ.
func (m *Manager) attachSource(rt *Runtime, id string, cfg Config, chGwCfg gocfg.Config, pl *ingestPipeline) {
	if !cfg.IsHLS() {
		w := dashingest.NewWatcher(chGwCfg, id, pl.headers, rt.Index, pl.broker)
		w.SetProgress(pl.progress)
		rt.Watcher = w
		rt.Source = w
		return
	}

	hw := hlsingest.NewWatcher(chGwCfg, id, cfg.MPDURL, pl.headers, cfg.DecryptOnIngest(),
		hlsingest.SourceStatePath(m.gwCfg.Store.DataDir, id), rt.Index, pl.broker)
	hw.SetProgress(pl.progress)
	rt.HLS = hw
	rt.Source = hw

	// One key cache per channel, shared by decrypt-on-ingest and the key proxy,
	// so a key is fetched from the upstream server at most once. Passthrough
	// channels persist keys to disk so playback survives the upstream key server
	// going away; decrypt channels never serve keys and keep them in memory only.
	keyStorePath := ""
	if !cfg.DecryptOnIngest() {
		keyStorePath = hlskey.KeyStorePath(m.gwCfg.Store.DataDir, id)
	}
	rt.Keys = hlskey.NewCache(nil, pl.headers, keyStorePath)
	pl.processor.SetKeyFetcher(rt.Keys)

	// HLS tracks are independent media playlists the player aligns itself, so
	// publish each segment directly rather than gating on the A/V barrier.
	pl.processor.SetPublishPolicy(pipeline.PublishImmediate)
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

	// Inject the saved upstream MPD so ParsedMPD() is non-nil and handleManifest
	// can serve requests without re-fetching. HLS channels have no MPD to
	// restore; their playlists are rendered from the index.
	if rt.Watcher != nil {
		upPath := filepath.Join(m.gwCfg.Store.DataDir, id, "upstream.mpd")
		if raw, err := os.ReadFile(upPath); err == nil {
			if p, err := imdp.Parse(raw); err == nil {
				rt.Watcher.SetParsedMPD(p, raw)
			} else {
				slog.Warn("VOD restore: upstream MPD parse failed", "channel", id, "err", err)
			}
		} else {
			slog.Warn("VOD restore: upstream MPD not found, manifest will be unavailable until re-fetch", "channel", id)
		}
	}

	slog.Info("channel restored in VOD mode", "id", id)
}

// launchIngest starts the channel's goroutines: the fetch pool, the processor,
// and the supervisor that keeps the source running.
//
// Every goroutine is counted in rt.wg, which is what lets Stop join them rather
// than merely cancel them — see Stop for why that matters.
func (m *Manager) launchIngest(ctx context.Context, rt *Runtime, cfg Config, pl *ingestPipeline) {
	// Register the VOD transition callback if requested. Only DASH signals the
	// end of a live stream this way (type dynamic→static); the HLS watcher
	// signals it by returning from Run when every variant reaches EXT-X-ENDLIST.
	if cfg.EnableVODTransition && rt.Watcher != nil {
		rt.Watcher.SetVODTransitionCallback(func(p *imdp.ParsedMPD) {
			rt.wg.Add(1)
			go func() { defer rt.wg.Done(); m.transitionToVOD(rt, p) }()
		})
	}

	rt.wg.Add(3)
	go func() { defer rt.wg.Done(); pl.pool.Run(ctx) }()
	go func() { defer rt.wg.Done(); pl.processor.Run(ctx, pl.pool) }()
	go func() { defer rt.wg.Done(); m.superviseSource(ctx, rt, cfg, pl) }()
}

// superviseSource runs the channel's source, restarting it with backoff until
// the context is cancelled or ingestion completes.
func (m *Manager) superviseSource(ctx context.Context, rt *Runtime, cfg Config, pl *ingestPipeline) {
	backoff := 2 * time.Second
	for {
		err := rt.Source.Run(ctx)
		if err == nil {
			// Source returned cleanly: ingestion is complete — a static MPD, or
			// an HLS playlist that reached EXT-X-ENDLIST. Enter StaticIngesting
			// mode and wait for the broker to drain before doing the full VOD
			// transition.
			if !cfg.IsVOD {
				rt.setStaticIngestStart(time.Now())
				rt.mode.Store(int32(ModeStaticIngesting))
				parsed := rt.ParsedMPD()
				rt.wg.Add(1)
				go func() {
					defer rt.wg.Done()
					m.awaitStaticIngestComplete(ctx, rt, pl.broker, parsed)
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
	mode := rt.Mode()
	if mode != ModeLive && mode != ModeStaticIngesting {
		return fmt.Errorf("channel %q is not in live or static-ingesting mode", id)
	}
	parsed := rt.ParsedMPD()
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); m.transitionToVOD(rt, parsed) }()
	return nil
}

// awaitStaticIngestComplete polls the broker until the queue drains (all
// static segments have been fetched), then triggers a VOD transition so that
// the channel is persisted as VOD and restartable from disk.
func (m *Manager) awaitStaticIngestComplete(ctx context.Context, rt *Runtime, broker *queue.Broker, parsedMPD *imdp.ParsedMPD) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if broker.Len() == 0 {
				slog.Info("static ingest complete, transitioning to VOD", "channel", rt.ID)
				m.transitionToVOD(rt, parsedMPD)
				return
			}
		}
	}
}

// transitionToVOD rebuilds a full-history index from disk using unbounded ring
// buffers, runs the A/V barrier, and switches the runtime to ModeVOD.
// Accepts channels in ModeLive or ModeStaticIngesting as the source state.
func (m *Manager) transitionToVOD(rt *Runtime, finalMPD *imdp.ParsedMPD) {
	if finalMPD != nil && finalMPD.MediaPresentationDuration > 0 {
		rt.finalDuration.Store(int64(finalMPD.MediaPresentationDuration))
	}

	// Accept transition from either ModeLive or ModeStaticIngesting.
	if !rt.mode.CompareAndSwap(int32(ModeLive), int32(ModeTransitioning)) &&
		!rt.mode.CompareAndSwap(int32(ModeStaticIngesting), int32(ModeTransitioning)) {
		return // already transitioning or in VOD
	}
	slog.Info("channel transitioning to VOD", "id", rt.ID)

	// Rebuild from disk with unbounded ring buffers so the VOD covers the whole
	// recorded history rather than just what the live ring still held. fMP4
	// timing comes from each segment's tfdt box and MPEG-TS timing from the
	// per-representation sidecar, so both formats reconstruct fully.
	vodCI := index.NewChannelIndex()
	if err := index.RebuildFromDiskForVOD(vodCI, m.gwCfg.Store.DataDir, rt.ID); err != nil {
		slog.Error("VOD rebuild failed", "channel", rt.ID, "err", err)
		rt.mode.Store(int32(ModeLive)) // rollback
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
		}
		if cb != nil {
			// Counted like every other channel goroutine: the callback snapshots
			// the final manifest into this channel's own data directory, so Stop
			// must not return while it is still writing there. Adding here is
			// safe because every path into transitionToVOD already runs inside a
			// counted goroutine.
			rt.wg.Add(1)
			go func() { defer rt.wg.Done(); cb(rt) }()
		}
	})
}

// RestartWithConfig stops the channel, persists cfg, and starts it again,
// restoring oldCfg in both the store and the manager if either step fails.
//
// It lives here rather than in the admin handler because it is a lifecycle
// operation over the manager's own state: it has to stop, persist and start as
// one unit, and decide what to roll back when a step in the middle fails.
//
// It requires SetStore. Persisting is half of what a restart means, so a
// manager without a store returns an error rather than restarting the channel
// and silently losing the new configuration on the next gateway restart.
//
// The manager's mutex is deliberately released before any of that runs. Stop
// and Start take it themselves, so holding it across them would deadlock — and
// even a lock held only for the duration would stall every HTTP handler, since
// they all reach their channel through Get.
func (m *Manager) RestartWithConfig(ctx context.Context, id string, cfg, oldCfg Config) error {
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return fmt.Errorf("no channel store attached")
	}

	_ = m.Stop(id)
	if err := store.Put(id, cfg); err != nil {
		if oldCfg.Enabled {
			_, _ = m.Start(ctx, id, oldCfg)
		}
		return fmt.Errorf("store error: %w", err)
	}
	if _, err := m.Start(ctx, id, cfg); err != nil {
		_ = store.Put(id, oldCfg)
		if oldCfg.Enabled {
			_, _ = m.Start(ctx, id, oldCfg)
		}
		return fmt.Errorf("start error: %w", err)
	}
	return nil
}

// ApplyConfig applies cfg to a running channel without restarting it, and
// reports the fields that prevented it from doing so.
//
// A non-empty result means the caller must stop and start the channel to make
// the edit take effect; ApplyConfig itself changes nothing in that case. The
// caller performs the restart rather than ApplyConfig, because Stop and Start
// take the manager's write lock that this method holds for reading.
//
// A channel that is not running also reports restart-required: there is no
// runtime to update, so the caller must Start it.
func (m *Manager) ApplyConfig(id string, cfg Config) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rt, ok := m.runtimes[id]
	if !ok {
		return []string{NotRunning}
	}
	if blockers := RestartRequired(rt.Config(), cfg, rt.Mode()); len(blockers) > 0 {
		return blockers
	}

	// Headers first: the provider is shared with the fetcher, watcher and key
	// cache, and a request that reads the new config should not still be able to
	// pick up the old credentials.
	rt.headers.Store(HeaderMap(cfg.FetchHeaders))
	rt.setConfig(cfg)
	slog.Info("channel config hot-applied", "id", id)
	return nil
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

func buildStatus(rt *Runtime) ChannelStatus {
	modeStr := "live"
	switch rt.Mode() {
	case ModeTransitioning:
		modeStr = "transitioning"
	case ModeVOD:
		modeStr = "vod"
	case ModeStaticIngesting:
		modeStr = "static_ingesting"
	}
	s := ChannelStatus{
		ID:      rt.ID,
		MPDURL:  rt.Config().MPDURL,
		Running: true,
		Mode:    modeStr,
	}
	ci := rt.ActiveIndex()
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
