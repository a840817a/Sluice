package index

import (
	"slices"
	"testing"
)

// HLS names periods by number. Sorted as strings, period 10 would come before
// period 2, and every consumer of the order would put a later stream first.
func TestSnapshotOrdersNumericPeriodsNumerically(t *testing.T) {
	ci := NewChannelIndex(PublishAVBarrier)
	for _, id := range []string{"10", "2", "0", "1"} {
		ci.Period(id).AS("0").Rep("v0", 8).RestorePublished(SegmentState{SegNo: 1, EndPTS: 1, Timescale: 1})
	}
	if got, want := ci.Snapshot().PeriodIDs(nil), []string{"0", "1", "2", "10"}; !slices.Equal(got, want) {
		t.Errorf("PeriodIDs = %v, want %v", got, want)
	}
}

// A DASH source's periods follow its MPD. One the MPD no longer lists has
// rolled off it, so it is older than every period listed and comes first;
// rolled-off periods order among themselves numbers numerically.
func TestOrderPeriodIDsFollowsTheDocument(t *testing.T) {
	got := OrderPeriodIDs([]string{"late", "10", "early", "2", "orphan"}, []string{"early", "late"})
	if want := []string{"2", "10", "orphan", "early", "late"}; !slices.Equal(got, want) {
		t.Errorf("OrderPeriodIDs = %v, want %v", got, want)
	}
}

// A period an origin restart started follows the period it restarted, and a
// later restart follows an earlier one, whether or not the MPD lists them.
func TestOrderPeriodIDsPlacesRestartsAfterTheirPeriod(t *testing.T) {
	ids := []string{"b", "a.r2", "a", "a.r1", "old.r1", "old", "a.r10"}
	got := OrderPeriodIDs(ids, []string{"a", "b"})
	if want := []string{"old", "old.r1", "a", "a.r1", "a.r2", "a.r10", "b"}; !slices.Equal(got, want) {
		t.Errorf("OrderPeriodIDs = %v, want %v", got, want)
	}
	// Not restart IDs: no number, a zero, a padded number.
	for _, id := range []string{"a.r", "a.r0", "a.r01", "a.rx"} {
		if base, gen := splitRestartPeriodID(id); base != id || gen != 0 {
			t.Errorf("splitRestartPeriodID(%q) = %q, %d; want itself, 0", id, base, gen)
		}
	}
	if id := RestartPeriodID("a", 3); id != "a.r3" {
		t.Errorf("RestartPeriodID = %q", id)
	}
	if gen, ok := RestartGeneration("a.r3", "a"); !ok || gen != 3 {
		t.Errorf("RestartGeneration(a.r3, a) = %d, %v", gen, ok)
	}
	if _, ok := RestartGeneration("b.r1", "a"); ok {
		t.Error("b.r1 counted as a restart of a")
	}
}

// The lifetime range must not shrink as the ring evicts: output numbers a
// later period's segments from it, and a shrinking range would renumber them
// while a live window slides.
func TestSegNoRangeSurvivesEviction(t *testing.T) {
	rep := NewChannelIndex(PublishAVBarrier).Period("0").AS("0").Rep("v0", 2)
	if _, _, ok := rep.SegNoRange(); ok {
		t.Fatal("an empty representation reported a range")
	}
	for n := uint64(10); n <= 14; n++ {
		rep.commit(SegmentState{SegNo: n, EndPTS: 1, Timescale: 1})
		rep.markPublished(map[uint64]struct{}{n: {}})
	}
	if first, last, ok := rep.SegNoRange(); !ok || first != 10 || last != 14 {
		t.Errorf("SegNoRange = %d..%d (%v), want 10..14 after evicting 10..12", first, last, ok)
	}

	// Committed but never published: in no playlist, so not in the range.
	rep.commit(SegmentState{SegNo: 15, EndPTS: 1, Timescale: 1})
	if _, last, _ := rep.SegNoRange(); last != 14 {
		t.Errorf("an unpublished segment widened the range to %d", last)
	}
}

// Like the range, the record of origin discontinuities outlives the ring:
// EXT-X-DISCONTINUITY-SEQUENCE must keep counting one whose segment has been
// evicted. Only publication records one, and a restored segment counts.
func TestDiscontinuitiesSurviveEviction(t *testing.T) {
	rep := NewChannelIndex(PublishAVBarrier).Period("0").AS("0").Rep("v0", 2)
	rep.RestorePublished(SegmentState{SegNo: 9, EndPTS: 1, Timescale: 1, Discontinuity: true})
	for n := uint64(10); n <= 14; n++ {
		rep.commit(SegmentState{SegNo: n, EndPTS: 1, Timescale: 1, Discontinuity: n == 11})
		rep.markPublished(map[uint64]struct{}{n: {}})
	}
	// Committed but never published: in no playlist, so not recorded.
	rep.commit(SegmentState{SegNo: 15, EndPTS: 1, Timescale: 1, Discontinuity: true})

	if got := rep.Discontinuities(); !slices.Equal(got, []uint64{9, 11}) {
		t.Errorf("Discontinuities = %v, want [9 11] after evicting both", got)
	}
}
