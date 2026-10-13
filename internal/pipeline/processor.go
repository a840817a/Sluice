package pipeline

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/metrics"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
)

const ringCapacity = 120

// keyFetchTimeout bounds a content-key request made while processing a segment.
const keyFetchTimeout = 15 * time.Second

// sidecarCompactEvery is how many appends to a representation's metadata file
// pass before it is rewritten. Large enough that compaction is rare, small
// enough that the file cannot grow without bound between restarts.
const sidecarCompactEvery = 2000

// SegmentCapacity returns the ring buffer capacity per representation.
// Unbounded (0) when the channel wants every segment kept; capped otherwise.
//
// It takes the flag rather than a config so the channel manager can call it
// while restoring an index from disk, before it has assembled the per-channel
// gateway config. Both callers must agree: a rebuilt index with a bounded ring
// and a processor with an unbounded one would silently drop history a
// KeepAllSegments channel is supposed to keep.
func SegmentCapacity(keepAllSegments bool) int {
	if keepAllSegments {
		return 0
	}
	return ringCapacity
}

func segmentCapacity(cfg config.Config) int {
	return SegmentCapacity(cfg.Store.KeepAllSegments)
}

// KeyFetcher supplies AES-128 content keys for segments that must be decrypted
// during ingest. It is nil for channels that never need one.
type KeyFetcher interface {
	Get(ctx context.Context, uri string) ([]byte, error)
}

// Processor drains fetch results: it retries failed fetches, hands fetched
// segments to its Intake, and records what became of them.
type Processor struct {
	cfg       config.Config
	ci        *index.ChannelIndex
	broker    *queue.Broker
	channelID string
	intake    *Intake
	inits     *initPreparer
	keys      KeyFetcher
	// warmedKeys tracks key server URIs already fetched into the key cache, so
	// a passthrough channel captures each key on disk exactly once while the
	// key server is still alive. Guarded by warmedMu because the reset-on-failure
	// happens in a background goroutine.
	warmedKeys map[string]bool
	warmedMu   sync.Mutex
	// warmWG counts in-flight key-warming goroutines so Run can join them before
	// returning. Without it a stopped channel keeps a key fetch — and its write
	// into the key cache — running for up to keyFetchTimeout.
	warmWG sync.WaitGroup
	// keyCtx roots every content-key request in the channel's lifetime. It is
	// replaced by Run with the context the channel was started under; the
	// Background default only applies to a Processor driven directly by a test.
	keyCtx context.Context
	// prog records ingest progress for the admin UI. Nil is valid and means
	// "record nothing" — every Counters method tolerates a nil receiver.
	prog *progress.Counters
}

// SetProgress attaches the channel's progress counters. Must be called before Run.
func (p *Processor) SetProgress(c *progress.Counters) {
	p.prog = c
	p.inits.prog = c
}

// NewProcessor creates a Processor.
func NewProcessor(cfg config.Config, channelID string, ci *index.ChannelIndex, broker *queue.Broker) *Processor {
	inits := newInitPreparer(cfg, ci)
	return &Processor{
		cfg:        cfg,
		ci:         ci,
		broker:     broker,
		channelID:  channelID,
		intake:     newIntake(cfg, channelID, ci, inits),
		inits:      inits,
		warmedKeys: make(map[string]bool),
		keyCtx:     context.Background(),
	}
}

// SetKeyFetcher attaches the key source used for decrypt-on-ingest channels.
// Must be called before Run.
func (p *Processor) SetKeyFetcher(k KeyFetcher) {
	p.keys = k
	p.intake.SetKeyFetcher(k)
}

// SetDecryptor attaches the registry of a DASH channel in key mode decrypt.
// Must be called before Run.
func (p *Processor) SetDecryptor(r *dashdecrypt.Registry) { p.inits.decrypt = r }

// Run starts consuming Results from pool until ctx is cancelled.
//
// It does not return until any key-warming goroutine it started has finished,
// so a caller that cancels ctx and waits for Run knows nothing is still writing
// to the channel's key cache.
func (p *Processor) Run(ctx context.Context, pool *fetch.Pool) {
	p.keyCtx = ctx
	p.inits.download = pool.Fetcher()
	defer p.warmWG.Wait()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	gate := initGate{
		limit:    p.cfg.Worker.FetchWorkers,
		initPath: func(task *queue.SegmentTask) string { return p.intake.location(task).InitPath() },
		prepare: func(ctx context.Context, task *queue.SegmentTask, path string) error {
			_, err := p.inits.prepare(ctx, task, path)
			return err
		},
		deliver: p.handle,
		abandon: func(res fetch.Result, err error) {
			p.prog.RecordSegmentError(StageInit, err.Error(), res.Task.RepID, res.Task.SegNo)
			slog.Warn("segment abandoned: init is not ready", "rep", res.Task.RepID, "seg", res.Task.SegNo, "err", err)
		},
		now:  time.Now,
		tick: ticker.C,
	}
	gate.run(ctx, pool.Results())
}

func (p *Processor) handle(res fetch.Result) {
	// Keep queue-depth gauge current each time a result is drained.
	metrics.BrokerDepth.WithLabelValues(p.channelID).Set(float64(p.broker.Len()))
	task := res.Task
	if res.Err != nil {
		slog.Warn("fetch error",
			"rep", task.RepID,
			"seg", task.SegNo,
			"url", task.URL,
			"err", res.Err,
		)
		metrics.SegmentsFetched.WithLabelValues(p.channelID, "error").Inc()
		p.prog.RecordSegmentError("fetch", res.Err.Error(), task.RepID, task.SegNo)
		switch res.StatusCode {
		case http.StatusGone: // 410: segment permanently unavailable, drop without retry.
			return
		case http.StatusTooEarly: // 425: segment not yet available, retry quickly without CDN rotation.
			res.Attempt.Retry(p.cfg.Worker.MaxRetriesStatic, 500*time.Millisecond)
			return
		}
		// Default: rotate to next CDN before requeueing (multi-BaseURL fallback).
		if len(task.FallbackURLs) > 0 {
			task.URL, task.FallbackURLs = task.FallbackURLs[0],
				append(task.FallbackURLs[1:], task.URL)
		}
		// Requeue for retry if within deadline and retry budget.
		res.Attempt.Retry(p.cfg.Worker.MaxRetriesStatic, 2*time.Second)
		return
	}

	// Passthrough stores the ciphertext as-is, but the key is captured into
	// the (disk-backed) cache now, while the key server is reachable, so the
	// channel can still serve the key after the key server goes away.
	if task.Encrypted && !task.DecryptOnIngest {
		p.warmKey(task.KeyURI)
	}

	out := p.intake.Store(p.keyCtx, task, res.Data)
	if !out.Stored {
		switch out.Stage {
		case StageDecrypt:
			metrics.SegmentsFetched.WithLabelValues(p.channelID, "decrypt_error").Inc()
		case StageWrite:
			metrics.SegmentsFetched.WithLabelValues(p.channelID, "write_error").Inc()
		}
		p.prog.RecordSegmentError(out.Stage, out.Err.Error(), task.RepID, task.SegNo)
		if out.Retry {
			res.Attempt.Retry(p.cfg.Worker.MaxRetriesStatic, time.Second)
		}
		return
	}
	metrics.SegmentsFetched.WithLabelValues(p.channelID, "ok").Inc()
	// Count the segment only if this number was not already tracked, so a
	// download bar cannot climb past its own total.
	if out.New {
		p.prog.AddStored(1, uint64(out.Bytes))
	}
	if out.Published > 0 {
		metrics.SegmentsPublished.WithLabelValues(p.channelID, task.MediaType).Add(float64(out.Published))
	}
}

// warmKey fetches a content key from the key server into the cache once, off
// the ingest path so a slow key server never stalls segment processing. For
// passthrough channels the cache is disk-backed, so this is what captures the
// key for restart recovery.
func (p *Processor) warmKey(uri string) {
	if uri == "" || p.keys == nil {
		return
	}
	p.warmedMu.Lock()
	if p.warmedKeys[uri] {
		p.warmedMu.Unlock()
		return
	}
	p.warmedKeys[uri] = true
	p.warmedMu.Unlock()

	p.warmWG.Add(1)
	go func() {
		defer p.warmWG.Done()
		ctx, cancel := context.WithTimeout(p.keyCtx, keyFetchTimeout)
		defer cancel()
		if _, err := p.keys.Get(ctx, uri); err != nil {
			slog.Warn("key warm failed", "channel", p.channelID, "err", err)
			// Allow a later segment to retry, since the key was not captured.
			p.warmedMu.Lock()
			p.warmedKeys[uri] = false
			p.warmedMu.Unlock()
		}
	}()
}
