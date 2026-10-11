package index

import "slices"

// PeriodRead is the published content of one representation in one period.
// Segments and Discontinuities are sorted by segment number and owned by this
// read. First/Last retain the lifetime published range after eviction. InitPath
// prefers published content, with a committed init as a fallback.
type PeriodRead struct {
	PeriodID        string
	Segments        []SegmentState
	First, Last     uint64
	RangeKnown      bool
	Discontinuities []uint64
	InitPath        string
}

type representationRead struct {
	published                    []SegmentState
	first, last                  uint64
	seen                         bool
	discontinuities              []uint64
	publishedInit, committedInit string
}

// read captures related fields under one representation lock. No returned slice
// (including a segment's IV) aliases the index. Other representations can change
// during a channel traversal; this is not a channel-wide atomic snapshot.
func (rs *RepresentationState) read() representationRead {
	rs.mu.Lock()
	all := rs.buf.Snapshot()
	r := representationRead{published: all[:0], first: rs.first, last: rs.last, seen: rs.seen}
	for _, seg := range all {
		switch seg.Status {
		case StatusPublished:
			seg.IV = slices.Clone(seg.IV)
			r.published = append(r.published, seg)
			if r.publishedInit == "" {
				r.publishedInit = seg.InitPath
			}
		case StatusCommitted:
			if seg.InitPath != "" {
				r.committedInit = seg.InitPath
			}
		}
	}
	for n := range rs.discontinuities {
		r.discontinuities = append(r.discontinuities, n)
	}
	rs.mu.Unlock()
	// Sorting touches only the detached copy, so publication need not wait.
	slices.Sort(r.discontinuities)
	slices.SortFunc(r.published, compareSegments)
	return r
}

func compareSegments(a, b SegmentState) int {
	if a.SegNo < b.SegNo {
		return -1
	}
	if a.SegNo > b.SegNo {
		return 1
	}
	return 0
}

// RepresentationPeriods reads repID across periods, without creating missing
// entries. It includes periods with an empty published window so their lifetime
// range and discontinuities can still number a later period's playlist. Periods
// use OrderPeriodIDs without a document; DASH callers apply their document order.
// Each representation is copied once under its own lock.
func (ci *ChannelIndex) RepresentationPeriods(repID string) []PeriodRead {
	byPeriod := make(map[string]*PeriodRead)
	publishedInits := make(map[string]string)
	var ids []string
	ci.ForEachRep(func(ref RepRef, rep *RepresentationState) {
		if ref.RepID != repID {
			return
		}
		r := rep.read()
		p := byPeriod[ref.PeriodID]
		if p == nil {
			p = &PeriodRead{PeriodID: ref.PeriodID}
			byPeriod[ref.PeriodID] = p
			ids = append(ids, ref.PeriodID)
		}
		p.Segments = append(p.Segments, r.published...)
		if r.seen {
			p.First, p.Last, p.RangeKnown = r.first, r.last, true
		}
		p.Discontinuities = append(p.Discontinuities, r.discontinuities...)
		// Preserve published priority even when more than one AdaptationSet
		// contains the same representation ID, as the URL omits the AS ID.
		if publishedInits[ref.PeriodID] == "" {
			publishedInits[ref.PeriodID] = r.publishedInit
		}
		if r.committedInit != "" {
			p.InitPath = r.committedInit
		}
	})
	out := make([]PeriodRead, 0, len(ids))
	for _, id := range OrderPeriodIDs(ids, nil) {
		p := *byPeriod[id]
		if publishedInits[id] != "" {
			p.InitPath = publishedInits[id]
		}
		slices.SortFunc(p.Segments, compareSegments)
		slices.Sort(p.Discontinuities)
		out = append(out, p)
	}
	return out
}

// TimelinePeriod is one period of a representation's Timeline: what it has
// published there, and what it ever has. Segments and Discontinuities are
// sorted by segment number and owned by the caller.
type TimelinePeriod struct {
	PeriodID string
	// Segments are the period's published segments still in the window,
	// possibly none.
	Segments []SegmentState
	// First and Last are the lowest and highest segment numbers the
	// representation has ever published in this period, kept after those
	// segments leave the window.
	First, Last uint64
	// Discontinuities are the numbers of every segment ever published in this
	// period that begins with an origin discontinuity, kept after those
	// segments leave the window.
	Discontinuities []uint64
	// InitPath is the period's init segment: a published segment's, or a
	// committed one's while none is published. Empty for formats without one.
	InitPath string
}

// Timeline returns repID's periods in timeline order (OrderPeriodIDs with
// docOrder, the source's document order; nil for a source without one). Only
// periods the representation has ever published in appear, each with its
// lifetime range, including those whose published window has emptied: a
// later period's numbering continues from them. A read of an unknown
// representation creates nothing.
//
// Each period is copied under its representation's lock; the Timeline as a
// whole is not one atomic read of the channel.
func (ci *ChannelIndex) Timeline(repID string, docOrder []string) []TimelinePeriod {
	byPeriod := make(map[string]*TimelinePeriod)
	publishedInits := make(map[string]string)
	seen := make(map[string]bool)
	var ids []string
	ci.ForEachRep(func(ref RepRef, rep *RepresentationState) {
		if ref.RepID != repID {
			return
		}
		r := rep.read()
		p := byPeriod[ref.PeriodID]
		if p == nil {
			p = &TimelinePeriod{PeriodID: ref.PeriodID}
			byPeriod[ref.PeriodID] = p
			ids = append(ids, ref.PeriodID)
		}
		// The same representation ID in two AdaptationSets of one period is
		// one track to a playlist, whose URLs omit the AdaptationSet: merge.
		p.Segments = append(p.Segments, r.published...)
		if r.seen {
			if !seen[ref.PeriodID] {
				p.First, p.Last = r.first, r.last
			}
			p.First, p.Last = min(p.First, r.first), max(p.Last, r.last)
			seen[ref.PeriodID] = true
		}
		p.Discontinuities = append(p.Discontinuities, r.discontinuities...)
		if publishedInits[ref.PeriodID] == "" {
			publishedInits[ref.PeriodID] = r.publishedInit
		}
		if r.committedInit != "" {
			p.InitPath = r.committedInit
		}
	})
	out := make([]TimelinePeriod, 0, len(ids))
	for _, id := range OrderPeriodIDs(ids, docOrder) {
		if !seen[id] {
			continue
		}
		p := *byPeriod[id]
		if publishedInits[id] != "" {
			p.InitPath = publishedInits[id]
		}
		slices.SortFunc(p.Segments, compareSegments)
		slices.Sort(p.Discontinuities)
		out = append(out, p)
	}
	return out
}

// PublishedSegmentPath resolves only a published media segment in the named
// period. Committed segments have not passed the A/V barrier and cannot be served.
// A read of an unknown key creates nothing.
func (ci *ChannelIndex) PublishedSegmentPath(periodID, repID string, segNo uint64) string {
	var path string
	ci.ForEachRepUntil(func(ref RepRef, rep *RepresentationState) bool {
		if ref.PeriodID != periodID || ref.RepID != repID {
			return true
		}
		rep.mu.Lock()
		if at, ok := rep.buf.pos[segNo]; ok {
			seg := rep.buf.buf[at]
			if seg.Status == StatusPublished {
				path = seg.Path
			}
		}
		rep.mu.Unlock()
		return path == ""
	})
	return path
}

// InitPath resolves an initialization section in the named period. Prefer any
// published init; if none exists, allow a committed init for init-only requests.
func (ci *ChannelIndex) InitPath(periodID, repID string) string {
	var published, committed string
	ci.ForEachRepUntil(func(ref RepRef, rep *RepresentationState) bool {
		if ref.PeriodID != periodID || ref.RepID != repID {
			return true
		}
		r := rep.read()
		published = r.publishedInit
		if r.committedInit != "" {
			committed = r.committedInit
		}
		return published == ""
	})
	if published != "" {
		return published
	}
	return committed
}
