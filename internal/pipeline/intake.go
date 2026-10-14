package pipeline

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/metrics"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
	"github.com/a840817a/sluice/internal/validate"
)

// Stages at which Intake.Store can give up on a segment. They are the stage
// names progress records errors under.
const (
	StageInit     = "init"
	StageDecrypt  = "decrypt"
	StageValidate = "validate"
	StageWrite    = "write"
)

// Outcome is what became of one fetched segment.
type Outcome struct {
	// Stored reports that the segment is on disk and committed to the index.
	Stored bool
	// New reports that the commit tracked a segment number the index did not
	// already hold. The HLS backfill path legitimately fetches a segment that
	// is still in flight, so a stored segment is not always a new one.
	New bool
	// Published is how many segments the commit made visible, this one and
	// any it released from the A/V barrier.
	Published int
	// Bytes is the size of what was stored: the cleartext, for a segment
	// decrypted during ingest.
	Bytes int
	// Stage and Err say where and why a segment that was not stored was
	// given up on. Retry reports that the failure was the gateway's own and
	// the segment is worth fetching again.
	Stage string
	Err   error
	Retry bool
}

// Intake turns a fetched segment into a stored one: it decrypts it when the
// channel decrypts on ingest, validates it, writes it and its init segment,
// works out its timing, records what the file cannot tell a restart, commits
// it to the channel index, and expires what has left the window.
//
// Its rules depend on one another, which is why they sit together: a segment
// is validated only after it is decrypted, ciphertext can only be checked for
// length, a segment decrypted on ingest carries no key material, timing uses
// the track timescale from the init segment's mdhd box, and the sidecar record
// is written before the commit makes the segment visible.
//
// An Intake is not safe for concurrent use; a Processor drives it from one
// goroutine.
type Intake struct {
	cfg       config.Config
	channelID string
	ci        *index.ChannelIndex
	keys      KeyFetcher
	// inits says whether a segment's init is usable, and its timescale.
	inits *initPreparer
	// sidecarAppends counts metadata records written per representation since
	// that representation's file was last compacted.
	sidecarAppends map[string]int
}

// NewIntake returns an Intake storing channelID's segments under cfg's data
// directory and committing them to ci.
func NewIntake(cfg config.Config, channelID string, ci *index.ChannelIndex) *Intake {
	return newIntake(cfg, channelID, ci, newInitPreparer(cfg, ci))
}

func newIntake(cfg config.Config, channelID string, ci *index.ChannelIndex, inits *initPreparer) *Intake {
	return &Intake{
		cfg:            cfg,
		channelID:      channelID,
		ci:             ci,
		inits:          inits,
		sidecarAppends: make(map[string]int),
	}
}

// SetKeyFetcher attaches the key source used for decrypt-on-ingest channels.
func (in *Intake) SetKeyFetcher(k KeyFetcher) { in.keys = k }

// Store stores task's segment, fetched as data. ctx bounds the content-key
// request a decrypt-on-ingest segment makes.
func (in *Intake) Store(ctx context.Context, task *queue.SegmentTask, data []byte) Outcome {
	// Decrypt before anything inspects the bytes, so that structural validation
	// runs on real media rather than ciphertext. Only decrypt-on-ingest channels
	// take this path; passthrough keeps the ciphertext all the way to disk.
	if task.Encrypted && task.DecryptOnIngest {
		plain, err := in.decrypt(ctx, task, data)
		if err != nil {
			slog.Warn("segment decryption failed",
				"rep", task.RepID,
				"seg", task.SegNo,
				"err", err,
			)
			return Outcome{Stage: StageDecrypt, Err: err}
		}
		data = plain
	}
	// A DASH decrypt channel decrypts the media of an AdaptationSet its
	// registry decrypted, in place, for the same reason.
	if task.Format == queue.FormatFMP4 && in.inits.decrypt != nil {
		as := dashdecrypt.ASKey{PeriodID: task.PeriodID, ASID: task.ASID}
		if tp, keys, ok := in.inits.decrypt.Lookup(as, task.RepID); ok {
			var used []cenc.KID
			lookup := func(kid cenc.KID) ([16]byte, bool) {
				used = append(used, kid)
				return keys(kid)
			}
			if err := cenc.DecryptSegment(data, tp, lookup); err != nil {
				slog.Warn("segment decryption failed",
					"rep", task.RepID,
					"seg", task.SegNo,
					"err", err,
				)
				// A segment whose samples rotate to a KID without a key is
				// dropped: its AdaptationSet is stored clear. It is fetched
				// again once the key is added (channel onKeysAdded).
				in.inits.decrypt.RecordSegmentFailure(as, task.RepID, task.SegNo, err)
				return Outcome{Stage: StageDecrypt, Err: err}
			}
			in.inits.decrypt.SegmentDecrypted(as, used)
		}
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
			return Outcome{Stage: StageValidate, Err: err}
		}
		startPTS = task.StartPTS
	case queue.FormatWebVTT:
		// Subtitle segments are text, not a media container: their timing comes
		// from the playlist EXTINF (like TS), and validation only confirms the
		// WebVTT signature so an origin error page is not stored as a cue.
		if err := validate.ValidateWebVTT(data); err != nil {
			slog.Warn("WebVTT segment validation failed",
				"rep", task.RepID, "seg", task.SegNo, "err", err)
			return Outcome{Stage: StageValidate, Err: err}
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
			return Outcome{Stage: StageValidate, Err: err}
		}
		startPTS = int64(segInfo.BaseMediaDecodeTime)
	}

	// No fMP4 media reaches its final path until its init is usable. The
	// processor prepares it before handing the segment over; a direct caller
	// gets StageInit until the init is on disk.
	var initTimescale uint32
	if task.Format == queue.FormatFMP4 {
		ts, err := in.inits.timescale(in.location(task).InitPath())
		if err != nil {
			return Outcome{Stage: StageInit, Err: err, Retry: true}
		}
		initTimescale = ts
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
	segPath := in.location(task).SegmentPath(task.SegNo, segExt)
	writeStart := time.Now()
	if err := store.Write(segPath, data); err != nil {
		slog.Warn("segment write failed, requeueing", "path", segPath, "err", err)
		return Outcome{Stage: StageWrite, Err: err, Retry: true}
	}
	metrics.SegmentWriteDuration.Observe(time.Since(writeStart).Seconds())

	// fMP4 uses the validated init timescale. TS/WebVTT use task timing.
	// This matters when the SegmentTemplate omits timescale (defaulting to 1 per
	// spec) but the track's BaseMediaDecodeTime is in a different timescale (e.g.
	// 48000 for audio, 90000 for video), which would cause A/V barrier to fail.
	trackTS := task.Timescale
	if initTimescale != 0 {
		trackTS = initTimescale
	}

	// Compute EndPTS: convert the SegmentTemplate duration (in SegmentTemplate
	// timescale units) to the track timescale so StartSec/EndSec are in seconds.
	endPTS := int64(0)
	if task.Duration > 0 && task.Timescale > 0 && trackTS > 0 {
		segSec := float64(task.Duration) / float64(task.Timescale)
		endPTS = startPTS + int64(segSec*float64(trackTS))
	}

	// MPEG-TS and WebVTT segments are self-contained: there is no initialization
	// segment, so the path stays empty and the init endpoint correctly 404s.
	initPath := ""
	if task.Format == queue.FormatFMP4 {
		initPath = in.location(task).InitPath()
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
	// Persist what the segment file itself cannot tell us, so the channel's DVR
	// window survives a restart. HLS opts in for every segment: MPEG-TS needs
	// its whole timing recorded, and even fMP4 (whose timing is in the tfdt box)
	// needs its discontinuity flag and key material preserved.
	if task.PersistMeta {
		in.persistSegmentMeta(task, seg)
	}

	// Then commit it to the channel index, which publishes what its policy
	// allows, so the record is appended before the segment is visible. Use
	// ASID as the AS key so multiple ASes of the same MediaType are distinct.
	committed := in.ci.Commit(index.RepRef{
		PeriodID: task.PeriodID, ASID: task.ASID, RepID: task.RepID, MediaType: task.MediaType,
	}, segmentCapacity(in.cfg), seg)

	// Expire segments older than the sliding window.
	in.expireOldSegments(in.ci.Period(task.PeriodID))

	return Outcome{Stored: true, New: committed.New, Published: committed.Published, Bytes: len(data)}
}

// expiryCutoff returns the media-time boundary below which published segments
// have left the window, and whether any expiry should happen at all.
//
// It is separated from the walk because it is the whole of the decision: given
// a live edge, everything about which segments age out is arithmetic on the
// configuration, with no index and no filesystem involved.
//
// The cutoff is media time, not wall clock. A segment's EndSec comes from
// tfdt/timescale — presentation time — so the window has to be measured from
// the period's own live edge or a channel whose origin's clock differs from
// ours expires the wrong segments.
func expiryCutoff(cfg config.Config, liveEdgeSec float64) (float64, bool) {
	// Keeping every segment outranks the cleanup mode: a channel that will
	// finish as VOD must not shed the history it is about to serve.
	if cfg.Store.KeepAllSegments {
		return 0, false
	}
	depth := cfg.Window.Depth
	if depth <= 0 {
		return 0, false
	}
	if liveEdgeSec <= 0 {
		return 0, false
	}
	cutoff := liveEdgeSec - (depth + cfg.Window.SafeEdgeBuffer).Seconds()
	if cutoff <= 0 {
		return 0, false
	}
	return cutoff, true
}

func (in *Intake) expireOldSegments(ps *index.PeriodState) {
	cutoff, ok := expiryCutoff(in.cfg, periodLiveEdgeSec(ps))
	if !ok {
		return
	}

	// The walk holds the period and AdaptationSet read locks, so it collects
	// what to delete and does not delete it: os.Remove is a syscall that can
	// block on a slow or failing filesystem, and blocking here stalls every
	// reader of this period — including the handlers serving the manifest.
	var toDelete []string
	ps.ForEachRep(func(_, _ string, rep *index.RepresentationState) {
		switch in.cfg.Store.Cleanup {
		case config.CleanupOnExpire:
			// Collecting paths and expiring under the representation's own lock
			// keeps the two atomic, so no segment is expired without its path
			// being handed back exactly once.
			toDelete = append(toDelete, rep.ExpireAndCollectPaths(cutoff)...)
		default:
			// "disabled" (and any unrecognized mode): only update the index,
			// never delete from disk.
			rep.ExpireOlderThan(cutoff)
		}
	})

	for _, path := range toDelete {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("cleanup: failed to remove expired segment",
				"path", path, "err", err)
		}
	}
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
func (in *Intake) decrypt(ctx context.Context, task *queue.SegmentTask, data []byte) ([]byte, error) {
	if in.keys == nil {
		return nil, errors.New("no key fetcher configured for an encrypted channel")
	}
	if task.KeyURI == "" {
		return nil, errors.New("encrypted segment has no key URI")
	}
	// The fetch is bounded so a hung key server cannot stall a worker forever,
	// and rooted at the channel's context so stopping the channel does not have
	// to wait out that bound.
	ctx, cancel := context.WithTimeout(ctx, keyFetchTimeout)
	defer cancel()

	key, err := in.keys.Get(ctx, task.KeyURI)
	if err != nil {
		return nil, fmt.Errorf("fetch key: %w", err)
	}
	return hlskey.Decrypt(data, key, task.IV)
}

// persistSegmentMeta appends this segment's metadata to its representation's
// sidecar, and occasionally compacts the file.
func (in *Intake) persistSegmentMeta(task *queue.SegmentTask, seg index.SegmentState) {
	path := in.location(task).SidecarPath()

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
	in.sidecarAppends[key]++
	if in.sidecarAppends[key] < sidecarCompactEvery {
		return
	}
	in.sidecarAppends[key] = 0

	dir := filepath.Dir(path)
	// Keep a record only while its segment is still on disk, in whatever
	// format this representation stores.
	if err := store.CompactSegmentMeta(path, func(segNo uint64) bool {
		return store.SegmentOnDisk(dir, segNo)
	}); err != nil {
		slog.Warn("segment metadata compaction failed", "path", path, "err", err)
	}
}

// location places task's representation under the gateway's data directory.
func (in *Intake) location(task *queue.SegmentTask) store.RepLocation {
	return store.RepLocation{
		DataDir: in.cfg.Store.DataDir, ChannelID: in.channelID,
		PeriodID: task.PeriodID, MediaType: task.MediaType, ASID: task.ASID, RepID: task.RepID,
	}
}
