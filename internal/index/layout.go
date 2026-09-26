package index

import "slices"

// PeriodLayout is one period of an index as Layout saw it.
type PeriodLayout struct {
	ID             string
	AdaptationSets []AdaptationSetLayout
}

// AdaptationSetLayout is one AdaptationSet of a PeriodLayout.
type AdaptationSetLayout struct {
	ID string
	// MediaType is "" until the set's first segment commits.
	MediaType string
	Reps      []RepLayout
}

// RepLayout is one representation of an AdaptationSetLayout: how many of the
// segments it holds are in each status.
type RepLayout struct {
	ID                            string
	Published, Committed, Expired int
}

// Layout returns the index's periods, AdaptationSets and representations, with
// each representation's segment counts, read under the index's own locks. It
// includes what ForEachRep never yields: a period with no AdaptationSets yet,
// which serving a manifest creates before any segment commits, and an
// AdaptationSet with no representations, which the rebuild creates before it
// reads the set's directory. Every level is ordered by ID.
func (ci *ChannelIndex) Layout() []PeriodLayout {
	ci.mu.RLock()
	defer ci.mu.RUnlock()
	out := make([]PeriodLayout, 0, len(ci.periods))
	for id, ps := range ci.periods {
		out = append(out, ps.layout(id))
	}
	slices.SortFunc(out, func(a, b PeriodLayout) int { return compareIDs(a.ID, b.ID) })
	return out
}

func (ps *PeriodState) layout(id string) PeriodLayout {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	pl := PeriodLayout{ID: id, AdaptationSets: make([]AdaptationSetLayout, 0, len(ps.adaptationSets))}
	for asID, as := range ps.adaptationSets {
		as.mu.RLock()
		al := AdaptationSetLayout{ID: asID, MediaType: as.mediaType, Reps: make([]RepLayout, 0, len(as.reps))}
		for repID, rep := range as.reps {
			p, c, e := rep.StatusCounts()
			al.Reps = append(al.Reps, RepLayout{ID: repID, Published: p, Committed: c, Expired: e})
		}
		as.mu.RUnlock()
		slices.SortFunc(al.Reps, func(a, b RepLayout) int { return compareIDs(a.ID, b.ID) })
		pl.AdaptationSets = append(pl.AdaptationSets, al)
	}
	slices.SortFunc(pl.AdaptationSets, func(a, b AdaptationSetLayout) int { return compareIDs(a.ID, b.ID) })
	return pl
}

func compareIDs(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
