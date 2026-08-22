package pipeline

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/metrics"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
	"github.com/a840817a/sluice/internal/validate"
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
// It takes the two flags rather than a config so the channel manager can call
// it while restoring an index from disk, before it has assembled the
// per-channel gateway config. Both callers must agree: a rebuilt index with a
// bounded ring and a processor with an unbounded one would silently drop
// history a KeepAllSegments channel is supposed to keep.
func SegmentCapacity(keepAllSegments, enableVODTransition bool) int {
	if keepAllSegments || enableVODTransition {
		return 0
	}
	return ringCapacity
}

func segmentCapacity(cfg config.Config) int {
	return SegmentCapacity(cfg.Store.KeepAllSegments, cfg.Store.EnableVODTransition)
}

// KeyFetcher supplies AES-128 content keys for segments that must be decrypted
// during ingest. It is nil for channels that never need one.
type KeyFetcher interface {
	Get(ctx context.Context, uri string) ([]byte, error)
}

// Processor drains fetch results, validates, writes to disk, and commits to index.
type Processor struct {
	cfg           config.Config
	ci            *index.ChannelIndex
	broker        *queue.Broker
	channelID     string
	barriers      map[string]*index.Barrier // periodID → Barrier
	repTimescales map[string]uint32         // repKey → actual track timescale from mdhd
	keys          KeyFetcher
	// sidecarAppends counts metadata records written per representation since
	// that representation's file was last compacted.
	sidecarAppends map[string]int
	// warmedKeys tracks upstream key URIs already fetched into the key cache, so
	// a passthrough channel captures each key on disk exactly once while the
	// upstream is still alive. Guarded by warmedMu because the reset-on-failure
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
	// publish decides when a committed segment becomes visible; see publish.go.
	publish PublishPolicy
	// prog records ingest progress for the admin UI. Nil is valid and means
	// "record nothing" — every Counters method tolerates a nil receiver.
	prog *progress.Counters
}

// SetProgress attaches the channel's progress counters. Must be called before Run.
func (p *Processor) SetProgress(c *progress.Counters) { p.prog = c }

// NewProcessor creates a Processor.
func NewProcessor(cfg config.Config, channelID string, ci *index.ChannelIndex, broker *queue.Broker) *Processor {
	return &Processor{
		cfg:            cfg,
		ci:             ci,
		broker:         broker,
		channelID:      channelID,
		barriers:       make(map[string]*index.Barrier),
		repTimescales:  make(map[string]uint32),
		sidecarAppends: make(map[string]int),
		warmedKeys:     make(map[string]bool),
		keyCtx:         context.Background(),
	}
}

// SetKeyFetcher attaches the key source used for decrypt-on-ingest channels.
// Must be called before Run.
func (p *Processor) SetKeyFetcher(k KeyFetcher) { p.keys = k }

// Run starts consuming Results from pool until ctx is cancelled.
//
// It does not return until any key-warming goroutine it started has finished,
// so a caller that cancels ctx and waits for Run knows nothing is still writing
// to the channel's key cache.
func (p *Processor) Run(ctx context.Context, pool *fetch.Pool) {
	p.keyCtx = ctx
	defer p.warmWG.Wait()
	results := pool.Results()
	for {
		select {
		case <-ctx.Done():
			return
		case res, ok := <-results:
			if !ok {
				return
			}
			p.handle(res)
		}
	}
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
			p.broker.Requeue(task, p.cfg.Worker.MaxRetriesStatic, 500*time.Millisecond)
			return
		}
		// Default: rotate to next CDN before requeueing (multi-BaseURL fallback).
		if len(task.FallbackURLs) > 0 {
			task.URL, task.FallbackURLs = task.FallbackURLs[0],
				append(task.FallbackURLs[1:], task.URL)
		}
		// Requeue for retry if within deadline and retry budget.
		p.broker.Requeue(task, p.cfg.Worker.MaxRetriesStatic, 2*time.Second)
		return
	}

	// Decrypt before anything inspects the bytes, so that structural validation
	// runs on real media rather than ciphertext. Only decrypt-on-ingest channels
	// take this path; passthrough keeps the ciphertext all the way to disk.
	data := res.Data
	if task.Encrypted && task.DecryptOnIngest {
		plain, err := p.decrypt(task, data)
		if err != nil {
			slog.Warn("segment decryption failed",
				"rep", task.RepID,
				"seg", task.SegNo,
				"err", err,
			)
			metrics.SegmentsFetched.WithLabelValues(p.channelID, "decrypt_error").Inc()
			p.prog.RecordSegmentError("decrypt", err.Error(), task.RepID, task.SegNo)
			return
		}
		data = plain
	} else if task.Encrypted {
		// Passthrough: the ciphertext is stored as-is, but the key is captured
		// into the (disk-backed) cache now, while the upstream is reachable, so
		// the channel can still serve the key after the upstream goes away.
		p.warmKey(task.KeyURI)
	}

	// Validate the media segment and establish its start time.
	//
	// fMP4 carries authoritative timing in its tfdt box. MPEG-TS has no such
	// box, so the start time is computed by the source from the playlist's
	// EXTINF durations and arrives on the task.
	var startPTS int64
	switch task.Format {
	case queue.FormatTS:
		// Ciphertext must never be sync-byte checked — every valid encrypted
		// segment would be rejected. Only its length is verifiable.
		validateTS := validate.ValidateTS
		if task.Encrypted && !task.DecryptOnIngest {
			validateTS = validate.ValidateTSEncrypted
		}
		if err := validateTS(data); err != nil {
			slog.Warn("TS segment validation failed",
				"rep", task.RepID,
				"seg", task.SegNo,
				"err", err,
			)
			p.prog.RecordSegmentError("validate", err.Error(), task.RepID, task.SegNo)
			return
		}
		startPTS = task.StartPTS
	case queue.FormatWebVTT:
		// Subtitle segments are text, not a media container: their timing comes
		// from the playlist EXTINF (like TS), and validation only confirms the
		// WebVTT signature so an origin error page is not stored as a cue.
		if err := validate.ValidateWebVTT(data); err != nil {
			slog.Warn("WebVTT segment validation failed",
				"rep", task.RepID, "seg", task.SegNo, "err", err)
			p.prog.RecordSegmentError("validate", err.Error(), task.RepID, task.SegNo)
			return
		}
		startPTS = task.StartPTS
	default:
		segInfo, err := validate.ValidateSegment(data)
		if err != nil {
			slog.Warn("segment validation failed",
				"rep", task.RepID,
				"seg", task.SegNo,
				"err", err,
			)
			p.prog.RecordSegmentError("validate", err.Error(), task.RepID, task.SegNo)
			return
		}
		startPTS = int64(segInfo.BaseMediaDecodeTime)
	}

	// Write init segment atomically to disk (first time only).
	if res.InitData != nil {
		if err := validate.ValidateInit(res.InitData); err != nil {
			slog.Warn("init validation failed", "rep", task.RepID, "err", err)
		} else {
			initPath := store.InitPath(p.cfg.Store.DataDir, p.channelID, task.PeriodID, task.MediaType, task.ASID, task.RepID)
			if err := store.Write(initPath, res.InitData); err != nil {
				slog.Error("init write failed", "path", initPath, "err", err)
			} else {
				// Cache the actual track timescale from the init segment's mdhd box.
				// The SegmentTemplate timescale (task.Timescale) is for computing
				// segment boundaries in presentation time, but the BaseMediaDecodeTime
				// in tfdt uses the track's own timescale. Using the correct timescale
				// ensures A/V barrier PTS comparisons are in the same unit (seconds).
				if ts, err := validate.TimescaleFromInitBytes(res.InitData); err == nil && ts > 0 {
					p.repTimescales[task.RepKey()] = ts
				}
			}
		}
	}

	// Write media segment atomically to disk (timed for metrics).
	// Non-fMP4 segments keep their native extension so they are served untouched.
	segExt := store.ExtFMP4
	switch task.Format {
	case queue.FormatTS:
		segExt = store.ExtTS
	case queue.FormatWebVTT:
		segExt = store.ExtVTT
	}
	segPath := store.SegmentPathExt(p.cfg.Store.DataDir, p.channelID, task.PeriodID, task.MediaType, task.ASID, task.RepID, task.SegNo, segExt)
	writeStart := time.Now()
	if err := store.Write(segPath, data); err != nil {
		slog.Warn("segment write failed, requeueing", "path", segPath, "err", err)
		metrics.SegmentsFetched.WithLabelValues(p.channelID, "write_error").Inc()
		p.prog.RecordSegmentError("write", err.Error(), task.RepID, task.SegNo)
		p.broker.Requeue(task, p.cfg.Worker.MaxRetriesStatic, time.Second)
		return
	}
	metrics.SegmentWriteDuration.Observe(time.Since(writeStart).Seconds())
	metrics.SegmentsFetched.WithLabelValues(p.channelID, "ok").Inc()

	// Determine the actual track timescale. Prefer the cached value from the init
	// segment's mdhd box; fall back to task.Timescale (SegmentTemplate value).
	// This matters when the SegmentTemplate omits timescale (defaulting to 1 per
	// spec) but the track's BaseMediaDecodeTime is in a different timescale (e.g.
	// 48000 for audio, 90000 for video), which would cause A/V barrier to fail.
	trackTS := task.Timescale
	if ts, ok := p.repTimescales[task.RepKey()]; ok {
		trackTS = ts
	}

	// Compute EndPTS: convert the SegmentTemplate duration (in SegmentTemplate
	// timescale units) to the track timescale so StartSec/EndSec are in seconds.
	endPTS := int64(0)
	if task.Duration > 0 && task.Timescale > 0 && trackTS > 0 {
		segSec := float64(task.Duration) / float64(task.Timescale)
		endPTS = startPTS + int64(segSec*float64(trackTS))
	}

	// Commit segment to the channel index.
	// Use ASID as the AS key so multiple ASes of the same MediaType are distinct.
	periodState := p.ci.Period(task.PeriodID)
	as := periodState.AS(task.ASID)
	as.SetMediaType(task.MediaType)
	rep := as.Rep(task.RepID, segmentCapacity(p.cfg))

	// MPEG-TS and WebVTT segments are self-contained: there is no initialization
	// segment, so the path stays empty and the init endpoint correctly 404s.
	initPath := ""
	if task.Format == queue.FormatFMP4 {
		initPath = store.InitPath(p.cfg.Store.DataDir, p.channelID, task.PeriodID, task.MediaType, task.ASID, task.RepID)
	}
	seg := index.SegmentState{
		SegNo:         task.SegNo,
		StartPTS:      startPTS,
		EndPTS:        endPTS,
		Timescale:     trackTS,
		Path:          segPath,
		InitPath:      initPath,
		Discontinuity: task.Discontinuity,
	}
	// Key material is recorded only when the bytes on disk are still encrypted,
	// so the outbound playlist can re-advertise EXT-X-KEY. A segment decrypted
	// during ingest is cleartext and must carry none.
	if task.Encrypted && !task.DecryptOnIngest {
		seg.KeyURI = task.KeyURI
		seg.IV = task.IV
	}
	// Count the segment only if this number was not already tracked. The HLS
	// backfill path legitimately re-fetches a segment that is still in flight, so
	// counting every commit would let a download bar climb past its own total.
	if rep.Commit(seg) {
		p.prog.AddStored(1, uint64(len(data)))
	}

	// Persist what the segment file itself cannot tell us, so the channel's DVR
	// window survives a restart. HLS opts in for every segment: MPEG-TS needs
	// its whole timing recorded, and even fMP4 (whose timing is in the tfdt box)
	// needs its discontinuity flag and key material preserved.
	if task.PersistMeta {
		p.persistSegmentMeta(task, seg)
	}

	// Publish ready segments. HLS tracks are independent media playlists that
	// the player aligns itself, so an HLS channel publishes each committed
	// segment directly; gating audio behind video (or vice versa) via the A/V
	// barrier would only stall a track whose peer momentarily lags. DASH, whose
	// single MPD must present time-aligned tracks, keeps the barrier.
	prevPub := countPublished(periodState)
	if p.publish == PublishImmediate {
		rep.MarkPublished(map[uint64]struct{}{task.SegNo: {}})
	} else {
		p.barrier(task.PeriodID).CheckAndPublish()
	}
	newPub := countPublished(periodState) - prevPub
	if newPub > 0 {
		metrics.SegmentsPublished.WithLabelValues(p.channelID, task.MediaType).Add(float64(newPub))
	}

	// Expire segments older than the sliding window.
	p.expireOldSegments(periodState)
}

func countPublished(ps *index.PeriodState) int {
	total := 0
	ps.ForEachRep(func(_, _ string, rep *index.RepresentationState) {
		total += len(rep.Published())
	})
	return total
}

func (p *Processor) barrier(periodID string) *index.Barrier {
	if b, ok := p.barriers[periodID]; ok {
		return b
	}
	b := index.NewBarrier(p.ci.Period(periodID), 0.5)
	p.barriers[periodID] = b
	return b
}

func (p *Processor) expireOldSegments(ps *index.PeriodState) {
	// When VOD transition is enabled or the channel keeps all segments, skip
	// expiration so the full history stays available.
	if p.cfg.Store.EnableVODTransition || p.cfg.Store.KeepAllSegments {
		return
	}

	depth := p.cfg.Window.Depth
	safe := p.cfg.Window.SafeEdgeBuffer
	if depth <= 0 {
		return
	}
	// Expiry must be based on media timeline, not wall clock.
	// Segment EndSec is derived from tfdt/timescale (presentation time), so we
	// compute a media-time cutoff from the current live edge in this period.
	liveEdge := periodLiveEdgeSec(ps)
	if liveEdge <= 0 {
		return
	}
	cutoff := liveEdge - (depth + safe).Seconds()
	if cutoff <= 0 {
		return
	}

	// Note the os.Remove below runs with the period and AdaptationSet read locks
	// held. That was already true of the hand-rolled walk this replaced and is
	// preserved deliberately: collecting paths and deleting outside the walk is
	// a real improvement but a separate change, not something to smuggle into a
	// mechanical conversion.
	ps.ForEachRep(func(_, _ string, rep *index.RepresentationState) {
		switch p.cfg.Store.Cleanup {
		case config.CleanupOnExpire:
			// Collect paths and expire atomically, then delete from disk.
			paths := rep.ExpireAndCollectPaths(cutoff)
			for _, path := range paths {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					slog.Warn("cleanup: failed to remove expired segment",
						"path", path, "err", err)
				}
			}
		default:
			// "disabled" (and any unrecognized mode): only update the index,
			// never delete from disk.
			rep.ExpireOlderThan(cutoff)
		}
	})
}

// periodLiveEdgeSec returns the maximum EndSec among published segments in a
// period. 0 means no published segments yet.
func periodLiveEdgeSec(ps *index.PeriodState) float64 {
	liveEdge := 0.0
	ps.ForEachRep(func(_, _ string, rep *index.RepresentationState) {
		for _, seg := range rep.Published() {
			if end := seg.EndSec(); end > liveEdge {
				liveEdge = end
			}
		}
	})
	return liveEdge
}

// decrypt resolves the segment's content key and decrypts it in place.
func (p *Processor) decrypt(task *queue.SegmentTask, data []byte) ([]byte, error) {
	if p.keys == nil {
		return nil, errors.New("no key fetcher configured for an encrypted channel")
	}
	if task.KeyURI == "" {
		return nil, errors.New("encrypted segment has no key URI")
	}
	// The fetch is bounded so a hung key server cannot stall a worker forever,
	// and rooted at the channel's context so stopping the channel does not have
	// to wait out that bound.
	ctx, cancel := context.WithTimeout(p.keyCtx, keyFetchTimeout)
	defer cancel()

	key, err := p.keys.Get(ctx, task.KeyURI)
	if err != nil {
		return nil, fmt.Errorf("fetch key: %w", err)
	}
	return hlskey.Decrypt(data, key, task.IV)
}

// persistSegmentMeta appends this segment's metadata to its representation's
// sidecar, and occasionally compacts the file.
func (p *Processor) persistSegmentMeta(task *queue.SegmentTask, seg index.SegmentState) {
	path := store.SidecarPath(p.cfg.Store.DataDir, p.channelID,
		task.PeriodID, task.MediaType, task.ASID, task.RepID)

	m := store.SegmentMeta{
		SegNo:         seg.SegNo,
		StartPTS:      seg.StartPTS,
		EndPTS:        seg.EndPTS,
		Timescale:     seg.Timescale,
		Discontinuity: seg.Discontinuity,
		KeyURI:        seg.KeyURI,
	}
	if len(seg.IV) > 0 {
		m.IV = hex.EncodeToString(seg.IV)
	}

	if err := store.AppendSegmentMeta(path, m); err != nil {
		// Not fatal: the segment is already on disk and serving. Only restart
		// recovery degrades.
		slog.Warn("segment metadata append failed",
			"path", path, "seg", seg.SegNo, "err", err)
		return
	}

	// The sidecar grows by one record per segment forever, so compact it
	// periodically. Records whose segment file has been expired off disk are
	// dropped, which keeps the file proportional to what is actually retained.
	key := task.RepKey()
	p.sidecarAppends[key]++
	if p.sidecarAppends[key] < sidecarCompactEvery {
		return
	}
	p.sidecarAppends[key] = 0

	dir := filepath.Dir(path)
	if err := store.CompactSegmentMeta(path, func(segNo uint64) bool {
		// Keep a record only while its segment is still on disk. Both
		// extensions are checked because a representation's format is not
		// known here.
		for _, ext := range []string{store.ExtTS, store.ExtFMP4} {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d%s", segNo, ext))); err == nil {
				return true
			}
		}
		return false
	}); err != nil {
		slog.Warn("segment metadata compaction failed", "path", path, "err", err)
	}
}

// warmKey fetches an upstream content key into the cache once, off the ingest
// path so a slow key server never stalls segment processing. For passthrough
// channels the cache is disk-backed, so this is what captures the key for
// restart recovery.
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
