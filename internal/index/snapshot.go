package index

import (
	"sort"
	"time"
)

// Snapshot is a detached read model of one channel index, captured in a single
// traversal. Each representation's published segments are copied under its own
// lock; publication and expiry can proceed in other representations during the
// traversal, so this is not a channel-wide single instant.
//
// It contains published segments only, sorted ascending by SegNo. A committed
// segment has not passed the A/V barrier and must not be advertised. The snapshot
// owns its segment slices and IV bytes; nothing aliases live state.
type Snapshot struct {
	reps map[repKey]snapRep
	// keys is sorted, so iteration order is stable across snapshots of the same
	// index — generated output should not reshuffle between requests.
	keys      []repKey
	periodIDs []string // sorted
	// TakenAt is when the traversal ran. Useful in logs and tests; generation
	// does not read it.
	TakenAt time.Time
}

type repKey struct {
	periodID string
	asID     string
	repID    string
}

type snapRep struct {
	ref       RepRef
	published []SegmentState
}

// Snapshot copies every representation's published segments in one walk.
// ForEachRep keeps the structure stable; read locks each representation's data.
func (ci *ChannelIndex) Snapshot() Snapshot {
	s := Snapshot{
		reps:    make(map[repKey]snapRep),
		TakenAt: time.Now(),
	}
	periods := make(map[string]struct{})

	ci.ForEachRep(func(ref RepRef, rep *RepresentationState) {
		k := repKey{periodID: ref.PeriodID, asID: ref.ASID, repID: ref.RepID}
		published := rep.read().published
		s.reps[k] = snapRep{ref: ref, published: published}
		s.keys = append(s.keys, k)
		periods[ref.PeriodID] = struct{}{}
	})

	for id := range periods {
		s.periodIDs = append(s.periodIDs, id)
	}
	s.periodIDs = OrderPeriodIDs(s.periodIDs, nil)
	sort.Slice(s.keys, func(i, j int) bool {
		a, b := s.keys[i], s.keys[j]
		if a.periodID != b.periodID {
			return comparePeriodIDs(a.periodID, b.periodID) < 0
		}
		if a.asID != b.asID {
			return a.asID < b.asID
		}
		return a.repID < b.repID
	})
	return s
}

// Published returns the representation's published segments sorted ascending by
// SegNo, or nil when the snapshot has no such representation.
//
// A representation that exists with nothing published is indistinguishable from
// an absent one here, which every current caller wants; use HasRep when the
// difference matters.
//
// The returned slice belongs to the snapshot. Do not reorder or write to it.
func (s Snapshot) Published(periodID, asID, repID string) []SegmentState {
	r, ok := s.reps[repKey{periodID: periodID, asID: asID, repID: repID}]
	if !ok {
		return nil
	}
	return r.published
}

// HasRep reports whether the snapshot contains this representation at all,
// which is distinct from it containing one with zero published segments.
func (s Snapshot) HasRep(periodID, asID, repID string) bool {
	_, ok := s.reps[repKey{periodID: periodID, asID: asID, repID: repID}]
	return ok
}

// ForEachRep calls fn for every representation, in a stable order. Unlike the
// live walk it holds no locks, so fn may do anything.
func (s Snapshot) ForEachRep(fn func(ref RepRef, published []SegmentState)) {
	for _, k := range s.keys {
		r := s.reps[k]
		fn(r.ref, r.published)
	}
}

// PeriodIDs returns the periods that have at least one representation, in
// OrderPeriodIDs order without a document: period numbers numerically. That is the
// timeline order of an HLS source only; a DASH source's periods are ordered by
// its MPD (channel.Serving.OrderPeriods).
func (s Snapshot) PeriodIDs() []string {
	out := make([]string, len(s.periodIDs))
	copy(out, s.periodIDs)
	return out
}
