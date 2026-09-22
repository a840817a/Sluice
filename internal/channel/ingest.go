package channel

// Assembling a channel's ingest machinery: what gets built, from which config,
// and in what order.

import (
	"log/slog"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashingest"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
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
	index     *index.ChannelIndex
	headers   *fetch.AtomicHeaders
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

// restoreIndex rebuilds the channel's segment index from what is already on
// disk, so a restart resumes instead of re-fetching.
//
// A rebuild failure is logged and not returned: an empty index means the
// channel re-ingests, which is recoverable, whereas refusing to start leaves it
// down until someone intervenes.
func restoreIndex(chGwCfg gocfg.Config, id string, cfg Config) *index.ChannelIndex {
	ci := index.NewChannelIndex()
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
	fetcher := fetch.NewFetcher(nil, headers, chGwCfg.Worker.MaxSegmentBytes, chGwCfg.Worker.SegmentTimeout)
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
		broker:    broker,
		pool:      pool,
		processor: processor,
		progress:  prog,
	}
	p.attachSource(id, cfg, chGwCfg)
	return p
}

// attachSource picks the ingest driver for this channel's upstream protocol.
// Both feed the same broker, processor, store and index; only discovery and the
// publish rule differ.
//
// It fills in the pipeline rather than a Runtime, so a Runtime is built once,
// complete, instead of being mutated into shape after construction.
func (p *ingestPipeline) attachSource(id string, cfg Config, chGwCfg gocfg.Config) {
	if !cfg.IsHLS() {
		w := dashingest.NewWatcher(chGwCfg, id, p.headers, p.index, p.broker)
		w.SetProgress(p.progress)
		p.watcher = w
		p.source = w
		return
	}

	hw := hlsingest.NewWatcher(chGwCfg, id, cfg.MPDURL, p.headers, cfg.DecryptOnIngest(),
		hlsingest.SourceStatePath(chGwCfg.Store.DataDir, id), p.index, p.broker)
	hw.SetProgress(p.progress)
	p.hls = hw
	p.source = hw

	// One key cache per channel, shared by decrypt-on-ingest and the key proxy,
	// so a key is fetched from the upstream server at most once. Passthrough
	// channels persist keys to disk so playback survives the upstream key server
	// going away; decrypt channels never serve keys and keep them in memory only.
	keyStorePath := ""
	if !cfg.DecryptOnIngest() {
		keyStorePath = hlskey.KeyStorePath(chGwCfg.Store.DataDir, id)
	}
	p.keys = hlskey.NewCache(nil, p.headers, keyStorePath)
	p.processor.SetKeyFetcher(p.keys)

	// HLS tracks are independent media playlists the player aligns itself, so
	// publish each segment directly rather than gating on the A/V barrier.
	p.processor.SetPublishPolicy(pipeline.PublishImmediate)
}
