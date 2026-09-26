package index

import (
	"slices"
	"testing"
)

// HLS names periods by epoch. Sorted as strings, epoch 10 would come before
// epoch 2, and every consumer of the order would put a later stream first.
func TestSnapshotOrdersNumericPeriodsNumerically(t *testing.T) {
	ci := NewChannelIndex()
	for _, id := range []string{"10", "2", "0", "1"} {
		ci.Period(id).AS("0").Rep("v0", 8).RestorePublished(SegmentState{SegNo: 1, EndPTS: 1, Timescale: 1})
	}
	if got, want := ci.Snapshot().PeriodIDs(), []string{"0", "1", "2", "10"}; !slices.Equal(got, want) {
		t.Errorf("PeriodIDs = %v, want %v", got, want)
	}
}

// A DASH source's periods follow its MPD; any the MPD does not list follow,
// numbers numerically.
func TestOrderPeriodIDsFollowsTheDocument(t *testing.T) {
	got := OrderPeriodIDs([]string{"late", "10", "early", "2", "orphan"}, []string{"early", "late"})
	if want := []string{"early", "late", "2", "10", "orphan"}; !slices.Equal(got, want) {
		t.Errorf("OrderPeriodIDs = %v, want %v", got, want)
	}
}

// The lifetime range must not shrink as the ring evicts: output numbers a
// later period's segments from it, and a shrinking range would renumber them
// while a live window slides.
func TestSegNoRangeSurvivesEviction(t *testing.T) {
	rep := NewChannelIndex().Period("0").AS("0").Rep("v0", 2)
	if _, _, ok := rep.SegNoRange(); ok {
		t.Fatal("an empty representation reported a range")
	}
	for n := uint64(10); n <= 14; n++ {
		rep.Commit(SegmentState{SegNo: n, EndPTS: 1, Timescale: 1})
		rep.MarkPublished(map[uint64]struct{}{n: {}})
	}
	if first, last, ok := rep.SegNoRange(); !ok || first != 10 || last != 14 {
		t.Errorf("SegNoRange = %d..%d (%v), want 10..14 after evicting 10..12", first, last, ok)
	}

	// Committed but never published: in no playlist, so not in the range.
	rep.Commit(SegmentState{SegNo: 15, EndPTS: 1, Timescale: 1})
	if _, last, _ := rep.SegNoRange(); last != 14 {
		t.Errorf("an unpublished segment widened the range to %d", last)
	}
}

// Like the range, the record of upstream discontinuities outlives the ring:
// EXT-X-DISCONTINUITY-SEQUENCE must keep counting one whose segment has been
// evicted. Only publication records one, and a restored segment counts.
func TestDiscontinuitiesSurviveEviction(t *testing.T) {
	rep := NewChannelIndex().Period("0").AS("0").Rep("v0", 2)
	rep.RestorePublished(SegmentState{SegNo: 9, EndPTS: 1, Timescale: 1, Discontinuity: true})
	for n := uint64(10); n <= 14; n++ {
		rep.Commit(SegmentState{SegNo: n, EndPTS: 1, Timescale: 1, Discontinuity: n == 11})
		rep.MarkPublished(map[uint64]struct{}{n: {}})
	}
	// Committed but never published: in no playlist, so not recorded.
	rep.Commit(SegmentState{SegNo: 15, EndPTS: 1, Timescale: 1, Discontinuity: true})

	if got := rep.Discontinuities(); !slices.Equal(got, []uint64{9, 11}) {
		t.Errorf("Discontinuities = %v, want [9 11] after evicting both", got)
	}
}
