package index

import (
	"sort"
	"time"
)

// Snapshot is an immutable read model of one channel index, captured in a
// single traversal.
//
// It exists because manifest generation reads many representations and has to
// describe one coherent presentation. Reading them one at a time from the live
// index — each call taking its own lock — lets two representations in the same
// manifest come from two different moments, which surfaces as timelines that do
// not line up. Taking everything at once removes the question.
//
// It holds published segments only, which is all the output side reads: a
// committed-but-unpublished segment has not passed the A/V barrier and must not
// be advertised. The slices are the ones RingBuffer.Snapshot already allocates
// per call, so a Snapshot owns its data and nothing aliases live state.
//
// Segments come out sorted ascending by SegNo. Ingest fetches the live edge
// first, so ring-buffer order is roughly reversed, and every consumer had to
// re-sort. Sorting once here means consumers share one slice without each
// mutating it under the others.
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
//
// It is built on ForEachRep, which holds the channel lock for the whole
// traversal, so the result is a genuine single instant rather than a sequence
// of independent reads.
func (ci *ChannelIndex) Snapshot() Snapshot {
	s := Snapshot{
		reps:    make(map[repKey]snapRep),
		TakenAt: time.Now(),
	}
	periods := make(map[string]struct{})

	ci.ForEachRep(func(ref RepRef, rep *RepresentationState) {
		k := repKey{periodID: ref.PeriodID, asID: ref.ASID, repID: ref.RepID}
		published := rep.Published()
		sort.Slice(published, func(i, j int) bool {
			return published[i].SegNo < published[j].SegNo
		})
		s.reps[k] = snapRep{ref: ref, published: published}
		s.keys = append(s.keys, k)
		periods[ref.PeriodID] = struct{}{}
	})

	for id := range periods {
		s.periodIDs = append(s.periodIDs, id)
	}
	sort.Strings(s.periodIDs)
	sort.Slice(s.keys, func(i, j int) bool {
		a, b := s.keys[i], s.keys[j]
		if a.periodID != b.periodID {
			return a.periodID < b.periodID
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

// PeriodIDs returns the periods that have at least one representation, sorted.
func (s Snapshot) PeriodIDs() []string {
	out := make([]string, len(s.periodIDs))
	copy(out, s.periodIDs)
	return out
}
