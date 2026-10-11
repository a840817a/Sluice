package channel

// Assembling a channel's ingest machinery: what gets built, from which config,
// and in what order.

import (
	"context"
	"log/slog"
	"sync"
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

// ingestPipeline is everything one channel needs to ingest: the index segments
// land in, the queue that holds discovered work, the pool that fetches it, the
// processor that commits it, the counters they feed, and the source that
// discovers it in the first place.
//
// Building it is a single step so that "what a running channel is made of" is
// one thing to read rather than four phases spread across the Manager. What it
// deliberately does NOT own is the channel's lifecycle: starting the goroutines,
// supervising the source and transitioning to VOD stay with the Manager, which
// is what actually makes those decisions.
type ingestPipeline struct {
	index   *index.ChannelIndex
	headers *fetch.AtomicHeaders
	// manifests makes the MPD and playlist requests, media the segment, init
	// and key requests; both send headers.
	manifests *fetch.Origin
	media     *fetch.Origin
	broker    *queue.Broker
	pool      *fetch.Pool
	processor *pipeline.Processor
	progress  *progress.Counters

	// The ingest driver for this channel's protocol. Exactly one of watcher and
	// hls is non-nil, and source is whichever it is; keys exists only for HLS.
	source  Source
	watcher *dashingest.Watcher
	hls     *hlsingest.Watcher
	keys    *hlskey.Cache
}

// channelGatewayConfig merges gateway defaults with this channel's settings.
//
// The result is a copy: several fields are per-channel only (marked yaml:"-" on
// the config type) and exist so downstream constructors can read one config
// instead of a config plus a scattering of overrides.
func channelGatewayConfig(gwCfg gocfg.Config, cfg Config) gocfg.Config {
	chGwCfg := gwCfg
	chGwCfg.Upstream.MPDURL = cfg.MPDURL
	chGwCfg.Upstream.ForwardClientIP = cfg.ForwardClientIP
	chGwCfg.Store.KeepAllSegments = cfg.KeepsAllSegments()
	return chGwCfg
}

// publishPolicy is when the channel's index makes a committed segment visible.
// HLS tracks are independent media playlists the player aligns itself, so an
// HLS channel publishes each segment directly; a DASH MPD must present
// time-aligned tracks, so a DASH channel gates on the A/V barrier. Every index
// built for the channel uses it, so a rebuild shows what the live index did.
func publishPolicy(cfg Config) index.PublishPolicy {
	if cfg.IsHLS() {
		return index.PublishImmediately
	}
	return index.PublishAVBarrier
}

// restoreIndex rebuilds the channel's segment index from what is already on
// disk, so a restart resumes instead of re-fetching.
//
// A rebuild failure is logged and not returned: an empty index means the
// channel re-ingests, which is recoverable, whereas refusing to start leaves it
// down until someone intervenes.
func restoreIndex(chGwCfg gocfg.Config, id string, cfg Config) *index.ChannelIndex {
	ci := index.NewChannelIndex(publishPolicy(cfg))
	if cfg.IsVOD {
		// A channel already transitioned to VOD keeps its full history, so the
		// rebuild uses unbounded rings.
		if err := index.RebuildFromDiskForVOD(ci, chGwCfg.Store.DataDir, id); err != nil {
			slog.Warn("VOD disk rebuild failed", "channel", id, "err", err)
		}
		return ci
	}
	// The rebuilt index must use the same ring capacity the processor will, so
	// the rule lives in one place.
	capacity := pipeline.SegmentCapacity(cfg.KeepsAllSegments())
	if err := index.RebuildFromDiskWithCapacity(ci, chGwCfg.Store.DataDir, id, capacity); err != nil {
		slog.Warn("disk rebuild failed", "channel", id, "err", err)
	}
	return ci
}

// newIngestPipeline builds everything a channel needs to ingest: the restored
// index, the queue, the worker pool, the processor, the counters, and the
// source that drives them.
//
// It is one function rather than a sequence of steps on the Manager because the
// order inside it carries meaning that a reader should not have to reassemble
// from four call sites: the index is restored before the processor is built, so
// the two agree on ring capacity, and the source is attached last because it
// configures the processor it is handed.
func newIngestPipeline(gwCfg gocfg.Config, id string, cfg Config) *ingestPipeline {
	// One config from here down. The merged copy differs from the gateway's only
	// in the fields this channel overrides, so passing both would be two names
	// for the same values and an invitation to read the wrong one.
	chGwCfg := channelGatewayConfig(gwCfg, cfg)
	ci := restoreIndex(chGwCfg, id, cfg)

	// One provider shared by the fetcher, the watcher and the key cache, so a
	// credential rotation applied via UpdateConfig reaches all three at once.
	headers := fetch.NewAtomicHeaders(HeaderMap(cfg.FetchHeaders))

	broker := queue.NewBroker(4096)
	// The gateway auth header goes only to the MPD and playlist requests;
	// segments, inits and keys carry the channel's headers alone.
	manifests := fetch.NewOrigin(headers, chGwCfg.Upstream.AuthHeader, chGwCfg.Upstream.AuthValue)
	media := fetch.NewOrigin(headers, "", "")
	fetcher := fetch.NewFetcher(nil, media, chGwCfg.Worker.MaxSegmentBytes, chGwCfg.Worker.SegmentTimeout)
	pool := fetch.NewPool(fetcher, broker, chGwCfg.Worker.FetchWorkers)
	processor := pipeline.NewProcessor(chGwCfg, id, ci, broker)

	// Progress counts from this start onwards, so the segments the rebuild
	// restored from disk are seeded in rather than re-counted as fresh work.
	// They are seeded without throughput samples: they were not fetched during
	// this run, and treating them as such would render an absurd rate and ETA.
	prog := progress.New()
	prog.SeedStored(storedSegments(ci))
	processor.SetProgress(prog)

	p := &ingestPipeline{
		index:     ci,
		headers:   headers,
		manifests: manifests,
		media:     media,
		broker:    broker,
		pool:      pool,
		processor: processor,
		progress:  prog,
	}
	p.attachSource(id, cfg, chGwCfg)
	return p
}

// attachSource picks the ingest driver for this channel's source protocol.
// Both feed the same broker, processor, store and index; only discovery and the
// publish rule differ.
//
// It fills in the pipeline rather than a Runtime, so a Runtime is built once,
// complete, instead of being mutated into shape after construction.
func (p *ingestPipeline) attachSource(id string, cfg Config, chGwCfg gocfg.Config) {
	if !cfg.IsHLS() {
		w := dashingest.NewWatcher(chGwCfg, id, p.manifests, p.index, p.broker)
		w.SetProgress(p.progress)
		p.watcher = w
		p.source = dashSource{w}
		return
	}

	hw := hlsingest.NewWatcher(chGwCfg, id, cfg.MPDURL, p.manifests, cfg.DecryptOnIngest(),
		hlsingest.SourceStatePath(chGwCfg.Store.DataDir, id), p.index, p.broker)
	hw.SetProgress(p.progress)
	p.hls = hw

	// One key cache per channel, shared by decrypt-on-ingest and the key proxy,
	// so a key is fetched from the key server at most once. Passthrough
	// channels persist keys to disk so playback survives the key server
	// going away; decrypt channels never serve keys and keep them in memory only.
	keyStorePath := ""
	if !cfg.DecryptOnIngest() {
		keyStorePath = hlskey.KeyStorePath(chGwCfg.Store.DataDir, id)
	}
	p.keys = hlskey.NewCache(nil, p.media, keyStorePath)
	p.source = hlsSource{w: hw, keys: p.keys}
	p.processor.SetKeyFetcher(p.keys)
}

// ingestControl is the VOD transition's seam onto a channel's ingest: the three
// steps that bring it to rest, in the order the transition calls them.
//
// Each step is idempotent, because a retried transition runs all three again
// against ingest that an earlier attempt already brought to rest. Each returns
// false when ctx ends first, which is how Stop abandons a transition that has
// not reached publication.
type ingestControl interface {
	// StopAdmission stops the source from admitting new work and waits for it
	// to exit. It returns the last MPD the source observed (nil for HLS), read
	// only after the source exited so it reflects the final poll.
	StopAdmission(ctx context.Context) (finalMPD *imdp.ParsedMPD, ok bool)
	// AwaitDrain waits until every task already admitted, including any retry
	// the processor requeues, has reached a terminal state.
	AwaitDrain(ctx context.Context) bool
	// Join stops the fetch pool and waits for the pool and the processor to
	// exit. The processor is not cancelled: it finishes its last result and
	// waits for key warming, so a passthrough key cache is not abandoned just
	// before the VOD needs to serve it offline.
	Join(ctx context.Context) bool
}

// drainCheckInterval is how often a channel whose source has finished is polled
// for the pipeline going idle. It can be short because Broker.Idle answers the
// question exactly: it used to be two seconds, which was not a poll interval so
// much as a grace period long enough for in-flight segments to land before an
// empty queue was mistaken for a finished ingest.
const drainCheckInterval = 100 * time.Millisecond

// channelIngest is the production ingestControl: the cancel functions and exit
// signals of a running channel's source and pipeline. launch runs the
// goroutines the signals describe and closes each one when they exit; nothing
// else can, since nothing else runs them.
type channelIngest struct {
	sourceCancel context.CancelFunc
	poolCancel   context.CancelFunc
	sourceDone   chan struct{} // closed after Source.Run has returned for good
	pipelineDone chan struct{} // closed after the pool and processor have exited
	broker       *queue.Broker
	parsedMPD    func() *imdp.ParsedMPD
}

func newChannelIngest(sourceCancel, poolCancel context.CancelFunc, broker *queue.Broker, parsedMPD func() *imdp.ParsedMPD) *channelIngest {
	return &channelIngest{
		sourceCancel: sourceCancel,
		poolCancel:   poolCancel,
		sourceDone:   make(chan struct{}),
		pipelineDone: make(chan struct{}),
		broker:       broker,
		parsedMPD:    parsedMPD,
	}
}

// launch runs the channel's ingest, counting every goroutine in wg: the fetch
// pool and the processor, whose exit closes pipelineDone, and the source,
// whose exit closes sourceDone. Each function must return once the context it
// was started under ends; they receive none here, because the caller built
// those contexts along with the cancel functions this ingest holds.
func (c *channelIngest) launch(wg *sync.WaitGroup, runPool, runProcessor, runSource func()) {
	// pipeline lets the VOD transition join just the pool and processor. It
	// must not be wg: the transition itself is counted there, so waiting on
	// it would make a Stop racing a VOD transition deadlock.
	var pipeline sync.WaitGroup
	pipeline.Add(2)
	wg.Add(4)
	go func() { defer wg.Done(); defer pipeline.Done(); runPool() }()
	go func() { defer wg.Done(); defer pipeline.Done(); runProcessor() }()
	go func() { defer wg.Done(); defer close(c.sourceDone); runSource() }()
	go func() {
		defer wg.Done()
		pipeline.Wait()
		close(c.pipelineDone)
	}()
}

func (c *channelIngest) StopAdmission(ctx context.Context) (*imdp.ParsedMPD, bool) {
	c.sourceCancel()
	select {
	case <-c.sourceDone:
	case <-ctx.Done():
		return nil, false
	}
	return c.parsedMPD(), ctx.Err() == nil
}

// AwaitDrain polls the broker for idleness. An idle broker is stronger than an
// empty queue: it also excludes a fetch, result, or write still in flight.
func (c *channelIngest) AwaitDrain(ctx context.Context) bool {
	ticker := time.NewTicker(drainCheckInterval)
	defer ticker.Stop()
	for {
		if c.broker.Idle() {
			return ctx.Err() == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// Join waits on pipelineDone rather than the runtime's WaitGroup: the VOD
// transition is itself counted there, so waiting on it would deadlock.
func (c *channelIngest) Join(ctx context.Context) bool {
	c.poolCancel()
	select {
	case <-c.pipelineDone:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// launchIngest starts the channel's goroutines: its ingest — the fetch pool,
// the processor, and the supervisor that keeps the source running — and the
// watch for a finished origin, which lives as long as the source does.
//
// Every goroutine is counted in rt.wg, which is what lets Stop join them rather
// than merely cancel them — see Stop for why that matters.
func (m *Manager) launchIngest(sourceCtx, poolCtx, processorCtx context.Context, rt *Runtime, cfg Config, pl *ingestPipeline, ingest *channelIngest) {
	ingest.launch(&rt.wg,
		func() { pl.pool.Run(poolCtx) },
		func() { pl.processor.Run(processorCtx, pl.pool) },
		func() { m.superviseSource(sourceCtx, rt, cfg) },
	)
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); m.awaitStaticOrigin(sourceCtx, rt) }()
}

// staticOriginPollInterval is how often a running channel is asked whether
// its origin turned out to be a finished asset.
const staticOriginPollInterval = time.Second

// awaitStaticOrigin moves a channel into ModeStaticIngesting as soon as the
// watcher has seen that the origin is a finished asset.
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
func (m *Manager) awaitStaticOrigin(ctx context.Context, rt *Runtime) {
	if rt.Source == nil {
		return
	}
	ticker := time.NewTicker(staticOriginPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !staticIngestVisible(rt.Source.FinishedAsset(), rt.autoVODAllowed()) {
				continue
			}
			if _, ok := rt.modes.PromoteStatic(); ok {
				rt.setStaticIngestStart(time.Now())
				slog.Info("upstream is a finished asset, ingesting", "channel", rt.ID)
			}
			return
		}
	}
}

// staticIngestVisible reports whether a live channel should already be shown as
// ingesting a finished asset: the origin has said so, and this channel is not
// one whose auto-VOD was deliberately suppressed.
func staticIngestVisible(originFinished, autoVODAllowed bool) bool {
	return originFinished && autoVODAllowed
}

// superviseSource runs the channel's source, restarting it with backoff until
// the context is cancelled or ingestion completes.
func (m *Manager) superviseSource(ctx context.Context, rt *Runtime, cfg Config) {
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
			// origin is expected to still be finished: fall through to the
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
			if cfg.IsVOD {
				return
			}
			// awaitStaticOrigin may have promoted the channel already.
			if from, ok := rt.modes.PromoteStatic(); ok || from == ModeStaticIngesting {
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
