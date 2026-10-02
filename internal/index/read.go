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
