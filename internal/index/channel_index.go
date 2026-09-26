// Package index maintains the in-memory segment state for each channel.
package index

import (
	"slices"
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
	periods map[string]*PeriodState
	// policy decides when a committed segment is published; see Commit.
	policy PublishPolicy
}

// NewChannelIndex returns an empty index that publishes under policy. The
// policy follows the channel's source protocol, and every index built for a
// channel — live, rebuilt at a restart, rebuilt for VOD — must use the same
// one, or segments a player has already seen can vanish when the index is
// rebuilt.
func NewChannelIndex(policy PublishPolicy) *ChannelIndex {
	return &ChannelIndex{periods: make(map[string]*PeriodState), policy: policy}
}

// CommitResult is what one Commit did.
type CommitResult struct {
	// New is false when the segment number was already tracked and its state
	// was replaced, so callers counting ingest progress must not count it a
	// second time.
	New bool
	// Published is how many segments, in any representation of the period,
	// the commit made visible to players.
	Published int
}

// Commit records a newly written segment in ref's representation, creating
// the period, AdaptationSet and representation (with ring capacity capacity)
// as needed and recording ref.MediaType, then publishes whatever the index's
// policy now allows.
func (ci *ChannelIndex) Commit(ref RepRef, capacity int, seg SegmentState) CommitResult {
	ps := ci.Period(ref.PeriodID)
	as := ps.AS(ref.ASID)
	if ref.MediaType != "" {
		as.SetMediaType(ref.MediaType)
	}
	rep := as.Rep(ref.RepID, capacity)
	res := CommitResult{New: rep.commit(seg)}
	res.Published = ci.publish(ps, map[*RepresentationState]map[uint64]struct{}{rep: {seg.SegNo: {}}})
	return res
}

// publishCommitted publishes what the policy allows of everything committed
// in ps. The rebuild commits a whole period first and publishes once, rather
// than running the barrier per segment, which would be quadratic in the
// period's length.
func (ci *ChannelIndex) publishCommitted(ps *PeriodState) {
	fresh := make(map[*RepresentationState]map[uint64]struct{})
	ps.ForEachRep(func(_, _ string, rep *RepresentationState) {
		for _, s := range rep.Committed() {
			if fresh[rep] == nil {
				fresh[rep] = make(map[uint64]struct{})
			}
			fresh[rep][s.SegNo] = struct{}{}
		}
	})
	ci.publish(ps, fresh)
}

// publish applies the index's policy to ps after fresh, the segments just
// committed there, and returns how many segments became visible.
// PublishImmediately publishes fresh itself; the A/V barrier ignores it and
// publishes whatever in the period it can now match.
func (ci *ChannelIndex) publish(ps *PeriodState, fresh map[*RepresentationState]map[uint64]struct{}) int {
	if ci.policy == PublishImmediately {
		n := 0
		for rep, segNos := range fresh {
			n += rep.markPublished(segNos)
		}
		return n
	}
	return ps.barrier.checkAndPublish()
}

// PeriodIDs returns the IDs of all known periods, including ones that hold no
// segment yet, in no particular order.
func (ci *ChannelIndex) PeriodIDs() []string {
	ci.mu.RLock()
	defer ci.mu.RUnlock()
	ids := make([]string, 0, len(ci.periods))
	for id := range ci.periods {
		ids = append(ids, id)
	}
	return ids
}

func (ci *ChannelIndex) Period(id string) *PeriodState {
	ci.mu.RLock()
	p, ok := ci.periods[id]
	ci.mu.RUnlock()
	if ok {
		return p
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if p, ok = ci.periods[id]; ok {
		return p
	}
	p = newPeriodState(id)
	ci.periods[id] = p
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
	for periodID, ps := range ci.periods {
		ps.mu.RLock()
		for asID, as := range ps.adaptationSets {
			as.mu.RLock()
			mediaType := as.mediaType
			for repID, rep := range as.reps {
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
	for periodID, ps := range ci.periods {
		ps.mu.RLock()
		for asID, as := range ps.adaptationSets {
			as.mu.RLock()
			mediaType := as.mediaType
			for repID, rep := range as.reps {
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
	for asID, as := range ps.adaptationSets {
		as.mu.RLock()
		mediaType := as.mediaType
		for _, rep := range as.reps {
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
	ps, ok := ci.periods[periodID]
	ci.mu.RUnlock()
	if !ok {
		return nil
	}
	ps.mu.RLock()
	as, ok := ps.adaptationSets[asID]
	ps.mu.RUnlock()
	if !ok {
		return nil
	}
	as.mu.RLock()
	defer as.mu.RUnlock()
	return as.reps[repID]
}

// PeriodState holds AdaptationSets for one period.
type PeriodState struct {
	mu             sync.RWMutex
	ID             string
	adaptationSets map[string]*AdaptationState
	// barrier publishes the period's segments under PublishAVBarrier.
	barrier *barrier
}

func newPeriodState(id string) *PeriodState {
	ps := &PeriodState{ID: id, adaptationSets: make(map[string]*AdaptationState)}
	ps.barrier = newBarrier(ps, 0.5)
	return ps
}

func (ps *PeriodState) AS(id string) *AdaptationState {
	ps.mu.RLock()
	as, ok := ps.adaptationSets[id]
	ps.mu.RUnlock()
	if ok {
		return as
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if as, ok = ps.adaptationSets[id]; ok {
		return as
	}
	as = &AdaptationState{ID: id, reps: make(map[string]*RepresentationState)}
	ps.adaptationSets[id] = as
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
	reps      map[string]*RepresentationState
}

// MediaType returns the AdaptationSet's media type, or "" before the first
// segment has been committed. Do not call it while already holding as.mu.
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
	r, ok := as.reps[id]
	as.mu.RUnlock()
	if ok {
		return r
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if r, ok = as.reps[id]; ok {
		return r
	}
	r = &RepresentationState{ID: id, buf: NewRingBuffer(capacity)}
	as.reps[id] = r
	return r
}

// RepresentationState holds the segment ring buffer for one representation.
type RepresentationState struct {
	mu  sync.Mutex
	ID  string
	buf *RingBuffer
	// first and last are the lowest and highest segment numbers this
	// representation has ever published. Unlike the ring they never shrink as
	// segments expire or are evicted, which is what lets output number a later
	// period's segments after this one's without the numbers moving as the
	// window slides. A restart recomputes them from what is on disk.
	//
	// Only publication widens them. A segment committed but never published —
	// its A/V partner never arrived before an upstream reset — is not in any
	// playlist, and counting it would open a gap in the numbering at the
	// period boundary.
	first, last uint64
	seen        bool
	// discontinuities holds the number of every segment this representation
	// has published that begins with an upstream discontinuity. Like the range
	// it outlives the ring: once such a segment leaves a live playlist,
	// EXT-X-DISCONTINUITY-SEQUENCE must still count its tag (RFC 8216
	// §6.2.2), and the segment itself is no longer there to say so.
	discontinuities map[uint64]struct{}
}

// recordPublished widens the lifetime range to seg and remembers a
// discontinuity it begins with. Caller holds rs.mu.
func (rs *RepresentationState) recordPublished(seg SegmentState) {
	if !rs.seen || seg.SegNo < rs.first {
		rs.first = seg.SegNo
	}
	if !rs.seen || seg.SegNo > rs.last {
		rs.last = seg.SegNo
	}
	rs.seen = true
	if seg.Discontinuity {
		if rs.discontinuities == nil {
			rs.discontinuities = make(map[uint64]struct{})
		}
		rs.discontinuities[seg.SegNo] = struct{}{}
	}
}

// SegNoRange returns the lowest and highest segment numbers this
// representation has ever published, and false if it has published none.
func (rs *RepresentationState) SegNoRange() (first, last uint64, ok bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.first, rs.last, rs.seen
}

// Discontinuities returns, in ascending order, the number of every segment
// this representation has ever published that begins with an upstream
// discontinuity. A restart recomputes them from what is on disk.
func (rs *RepresentationState) Discontinuities() []uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.discontinuities) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(rs.discontinuities))
	for n := range rs.discontinuities {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// RestorePublished adds a segment directly as StatusPublished, bypassing the
// A/V barrier. Used during startup index rebuild from disk — segments that
// were previously committed and published do not need re-validation.
func (rs *RepresentationState) RestorePublished(seg SegmentState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	seg.Status = StatusPublished
	rs.recordPublished(seg)
	rs.buf.Push(seg)
}

// commit records a newly written segment in the ring buffer, and reports
// whether its number was new. Publication is ChannelIndex.Commit's business.
func (rs *RepresentationState) commit(seg SegmentState) bool {
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

// markPublished transitions the given segment numbers from StatusCommitted to
// StatusPublished, and returns how many it transitioned.
func (rs *RepresentationState) markPublished(segNos map[uint64]struct{}) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	rs.buf.UpdateWhere(func(s *SegmentState) {
		if _, ok := segNos[s.SegNo]; ok && s.Status == StatusCommitted {
			s.Status = StatusPublished
			rs.recordPublished(*s)
			n++
		}
	})
	return n
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
