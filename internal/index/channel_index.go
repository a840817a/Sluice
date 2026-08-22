// Package index maintains the in-memory segment state for each channel.
package index

import (
	"sync"
	"time"
)

// SegmentStatus is the lifecycle state of one segment.
type SegmentStatus int8

const (
	StatusDiscovered SegmentStatus = iota
	StatusFetching
	StatusCommitted // written to disk, awaiting A/V barrier
	StatusPublished // included in live MPD window
	StatusExpired   // slid out of window but file may still be on disk
)

func (s SegmentStatus) String() string {
	switch s {
	case StatusDiscovered:
		return "DISCOVERED"
	case StatusFetching:
		return "FETCHING"
	case StatusCommitted:
		return "COMMITTED"
	case StatusPublished:
		return "PUBLISHED"
	case StatusExpired:
		return "EXPIRED"
	default:
		return "UNKNOWN"
	}
}

// SegmentState is metadata for one downloaded segment held in the ring buffer.
type SegmentState struct {
	SegNo     uint64
	StartPTS  int64 // baseMediaDecodeTime from tfdt (timescale units)
	EndPTS    int64 // StartPTS + duration (timescale units)
	Timescale uint32
	Path      string // committed file path
	InitPath  string // empty for self-contained segments (MPEG-TS)
	Status    SegmentStatus
	CommitAt  time.Time
	Retries   int
	// Discontinuity marks a timeline break immediately before this segment,
	// carried through from the source (HLS EXT-X-DISCONTINUITY) so the outbound
	// playlist can re-emit it.
	Discontinuity bool

	// KeyURI and IV describe the AES-128 key this segment is still encrypted
	// with, for segments stored as ciphertext (passthrough mode). Both are empty
	// for cleartext segments, including ones decrypted during ingest.
	KeyURI string
	IV     []byte
}

// StartSec and EndSec convert PTS to seconds using the segment's timescale.
func (s *SegmentState) StartSec() float64 {
	if s.Timescale == 0 {
		return 0
	}
	return float64(s.StartPTS) / float64(s.Timescale)
}
func (s *SegmentState) EndSec() float64 {
	if s.Timescale == 0 {
		return 0
	}
	return float64(s.EndPTS) / float64(s.Timescale)
}

// ChannelIndex is the top-level index for one channel.
// It is safe for concurrent use.
type ChannelIndex struct {
	mu      sync.RWMutex
	Periods map[string]*PeriodState
}

func NewChannelIndex() *ChannelIndex {
	return &ChannelIndex{Periods: make(map[string]*PeriodState)}
}

// Mu exposes the read-write mutex for callers that need to snapshot the Periods map.
func (ci *ChannelIndex) Mu() *sync.RWMutex { return &ci.mu }

// PeriodIDs returns the IDs of all known periods. Caller must hold at least ci.Mu().RLock().
func (ci *ChannelIndex) PeriodIDs() []string {
	ids := make([]string, 0, len(ci.Periods))
	for id := range ci.Periods {
		ids = append(ids, id)
	}
	return ids
}

func (ci *ChannelIndex) Period(id string) *PeriodState {
	ci.mu.RLock()
	p, ok := ci.Periods[id]
	ci.mu.RUnlock()
	if ok {
		return p
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if p, ok = ci.Periods[id]; ok {
		return p
	}
	p = newPeriodState(id)
	ci.Periods[id] = p
	return p
}

// The media types an AdaptationSet can carry, as stored on AdaptationState.
//
// These are declared here rather than reused from internal/mpd because the
// index is protocol-neutral — it holds HLS segments too, and hlsingest does not
// import the DASH parser at all. The DASH path converts mpd.MediaType to these
// strings; TestMediaTypeConstantsMatchMPD in internal/dashingest, the package
// that performs that conversion, pins the two vocabularies together.
//
// A mismatch would not fail to compile: barrier.go would simply stop
// recognising a track type, fall into its single-track-type path, and publish
// without A/V gating. Silent loss of alignment is the reason these are
// constants at all.
const (
	MediaVideo = "video"
	MediaAudio = "audio"
	MediaText  = "text"
)

// RepRef identifies one representation's position in the index.
type RepRef struct {
	PeriodID  string
	ASID      string
	RepID     string
	MediaType string // "" until the first segment of the AdaptationSet commits
}

// ForEachRep calls fn once for every representation in the index, taking the
// channel → period → AdaptationSet locks in that order.
//
// The nesting is the point: this index has three lock levels, and every caller
// that walked them by hand had to get the order right or risk a deadlock.
// Methods on the representation itself (Published, Committed, StatusCounts, …)
// take their own lock and are safe to call from fn; anything that locks the
// AdaptationSet — MediaType(), for one — is not, which is why the media type is
// passed in. fn must not add or remove periods, AdaptationSets, or reps.
func (ci *ChannelIndex) ForEachRep(fn func(ref RepRef, rep *RepresentationState)) {
	ci.mu.RLock()
	defer ci.mu.RUnlock()
	for periodID, ps := range ci.Periods {
		ps.mu.RLock()
		for asID, as := range ps.AdaptationSets {
			as.mu.RLock()
			mediaType := as.mediaType
			for repID, rep := range as.Reps {
				fn(RepRef{PeriodID: periodID, ASID: asID, RepID: repID, MediaType: mediaType}, rep)
			}
			as.mu.RUnlock()
		}
		ps.mu.RUnlock()
	}
}

// ForEachRepUntil is ForEachRep with early termination: fn returns false to
// stop the walk.
//
// It exists because the hand-rolled walks that needed to bail out early had to
// unlock the period by hand on the return path, which is one forgotten
// statement away from a stuck index. Here the walk owns every lock, so an early
// exit cannot leak one. The same constraints as ForEachRep apply to fn.
func (ci *ChannelIndex) ForEachRepUntil(fn func(ref RepRef, rep *RepresentationState) bool) {
	ci.mu.RLock()
	defer ci.mu.RUnlock()
	for periodID, ps := range ci.Periods {
		ps.mu.RLock()
		for asID, as := range ps.AdaptationSets {
			as.mu.RLock()
			mediaType := as.mediaType
			for repID, rep := range as.Reps {
				if !fn(RepRef{PeriodID: periodID, ASID: asID, RepID: repID, MediaType: mediaType}, rep) {
					as.mu.RUnlock()
					ps.mu.RUnlock()
					return
				}
			}
			as.mu.RUnlock()
		}
		ps.mu.RUnlock()
	}
}

// ForEachRep is the period-scoped sibling of ChannelIndex.ForEachRep, for
// callers that already hold a *PeriodState and only need the two levels below
// it. Same rule: fn runs with the AdaptationSet lock held, so the media type is
// passed in rather than fetched via as.MediaType().
func (ps *PeriodState) ForEachRep(fn func(asID, mediaType string, rep *RepresentationState)) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	for asID, as := range ps.AdaptationSets {
		as.mu.RLock()
		mediaType := as.mediaType
		for _, rep := range as.Reps {
			fn(asID, mediaType, rep)
		}
		as.mu.RUnlock()
	}
}

// FindRep returns the representation at the given position, or nil if any level
// of the path is absent. Unlike Period/AS/Rep it creates nothing, so it is the
// right call for a read-only lookup.
func (ci *ChannelIndex) FindRep(periodID, asID, repID string) *RepresentationState {
	ci.mu.RLock()
	ps, ok := ci.Periods[periodID]
	ci.mu.RUnlock()
	if !ok {
		return nil
	}
	ps.mu.RLock()
	as, ok := ps.AdaptationSets[asID]
	ps.mu.RUnlock()
	if !ok {
		return nil
	}
	as.mu.RLock()
	defer as.mu.RUnlock()
	return as.Reps[repID]
}

// PeriodState holds AdaptationSets for one period.
type PeriodState struct {
	mu             sync.RWMutex
	ID             string
	AdaptationSets map[string]*AdaptationState
}

func newPeriodState(id string) *PeriodState {
	return &PeriodState{ID: id, AdaptationSets: make(map[string]*AdaptationState)}
}

// Mu exposes the read-write mutex for callers that need to iterate AdaptationSets.
func (ps *PeriodState) Mu() *sync.RWMutex { return &ps.mu }

func (ps *PeriodState) AS(id string) *AdaptationState {
	ps.mu.RLock()
	as, ok := ps.AdaptationSets[id]
	ps.mu.RUnlock()
	if ok {
		return as
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if as, ok = ps.AdaptationSets[id]; ok {
		return as
	}
	as = &AdaptationState{ID: id, Reps: make(map[string]*RepresentationState)}
	ps.AdaptationSets[id] = as
	return as
}

// AdaptationState holds Representations for one AdaptationSet.
type AdaptationState struct {
	mu sync.RWMutex
	ID string
	// mediaType ("video" / "audio" / "text") is unexported because ingest
	// discovers it on the first committed segment while HTTP handlers and the
	// A/V barrier read it concurrently. Go through MediaType/SetMediaType, which
	// take the lock; code inside this package that already holds mu reads the
	// field directly.
	mediaType string
	Reps      map[string]*RepresentationState
}

// Mu exposes the read-write mutex for callers that need to iterate Reps.
func (as *AdaptationState) Mu() *sync.RWMutex { return &as.mu }

// MediaType returns the AdaptationSet's media type, or "" before the first
// segment has been committed. Do not call it while already holding as.Mu().
func (as *AdaptationState) MediaType() string {
	as.mu.RLock()
	defer as.mu.RUnlock()
	return as.mediaType
}

// SetMediaType records the AdaptationSet's media type.
func (as *AdaptationState) SetMediaType(mediaType string) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.mediaType = mediaType
}

func (as *AdaptationState) Rep(id string, capacity int) *RepresentationState {
	as.mu.RLock()
	r, ok := as.Reps[id]
	as.mu.RUnlock()
	if ok {
		return r
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if r, ok = as.Reps[id]; ok {
		return r
	}
	r = &RepresentationState{ID: id, buf: NewRingBuffer(capacity)}
	as.Reps[id] = r
	return r
}

// RepresentationState holds the segment ring buffer for one representation.
type RepresentationState struct {
	mu  sync.Mutex
	ID  string
	buf *RingBuffer
}

// RestorePublished adds a segment directly as StatusPublished, bypassing the
// A/V barrier. Used during startup index rebuild from disk — segments that
// were previously committed and published do not need re-validation.
func (rs *RepresentationState) RestorePublished(seg SegmentState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	seg.Status = StatusPublished
	rs.buf.Push(seg)
}

// Commit records a newly written segment in the ring buffer.
//
// It reports whether the segment was new. A false result means this segment
// number was already tracked and its state was replaced, so callers counting
// ingest progress must not count it a second time.
func (rs *RepresentationState) Commit(seg SegmentState) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	seg.Status = StatusCommitted
	seg.CommitAt = time.Now()
	return rs.buf.Push(seg)
}

// Published returns all segments currently in StatusPublished order by SegNo.
func (rs *RepresentationState) Published() []SegmentState {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	all := rs.buf.Snapshot()
	out := all[:0]
	for _, s := range all {
		if s.Status == StatusPublished {
			out = append(out, s)
		}
	}
	return out
}

// Committed returns all segments currently in StatusCommitted.
func (rs *RepresentationState) Committed() []SegmentState {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	all := rs.buf.Snapshot()
	out := all[:0]
	for _, s := range all {
		if s.Status == StatusCommitted {
			out = append(out, s)
		}
	}
	return out
}

// MarkPublished transitions the given segment numbers to StatusPublished.
func (rs *RepresentationState) MarkPublished(segNos map[uint64]struct{}) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.buf.UpdateWhere(func(s *SegmentState) {
		if _, ok := segNos[s.SegNo]; ok && s.Status == StatusCommitted {
			s.Status = StatusPublished
		}
	})
}

// ExpireOlderThan transitions segments whose EndSec is before cutoff to StatusExpired.
func (rs *RepresentationState) ExpireOlderThan(cutoffSec float64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.buf.UpdateWhere(func(s *SegmentState) {
		if s.Status == StatusPublished && s.EndSec() < cutoffSec {
			s.Status = StatusExpired
		}
	})
}

// ExpireAndCollectPaths transitions Published segments with EndSec < cutoffSec to
// StatusExpired and returns their on-disk Path values so the caller can delete them.
// Collecting paths before changing status avoids a race between status transition
// and path lookup.
func (rs *RepresentationState) ExpireAndCollectPaths(cutoffSec float64) []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var paths []string
	rs.buf.UpdateWhere(func(s *SegmentState) {
		if s.Status == StatusPublished && s.EndSec() < cutoffSec {
			if s.Path != "" {
				paths = append(paths, s.Path)
			}
			s.Status = StatusExpired
		}
	})
	return paths
}

// StatusCounts returns the number of segments in each meaningful status bucket.
func (rs *RepresentationState) StatusCounts() (published, committed, expired int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, s := range rs.buf.Snapshot() {
		switch s.Status {
		case StatusPublished:
			published++
		case StatusCommitted:
			committed++
		case StatusExpired:
			expired++
		}
	}
	return
}

// Total returns the total number of segments tracked in this representation.
func (rs *RepresentationState) Total() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.buf.Snapshot())
}

// MaxSegNo returns the highest segment number tracked in this representation, or 0 if empty.
func (rs *RepresentationState) MaxSegNo() uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var max uint64
	for _, s := range rs.buf.Snapshot() {
		if s.SegNo > max {
			max = s.SegNo
		}
	}
	return max
}

// TrackedSegNos returns the segment numbers currently tracked in this
// representation together with the highest seen segment number.
func (rs *RepresentationState) TrackedSegNos() (map[uint64]struct{}, uint64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	segNos := make(map[uint64]struct{})
	var max uint64
	for _, s := range rs.buf.Snapshot() {
		segNos[s.SegNo] = struct{}{}
		if s.SegNo > max {
			max = s.SegNo
		}
	}
	return segNos, max
}
