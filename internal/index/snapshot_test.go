package index

import (
	"testing"
)

// commitPublished puts n segments into one representation and publishes them,
// newest first — the order ingest actually produces, since it fetches the live
// edge before backfilling.
func commitPublished(t *testing.T, ci *ChannelIndex, periodID, asID, repID string, segNos ...uint64) {
	t.Helper()
	as := ci.Period(periodID).AS(asID)
	as.SetMediaType(MediaVideo)
	rep := as.Rep(repID, 0)
	published := make(map[uint64]struct{}, len(segNos))
	for _, n := range segNos {
		rep.Commit(SegmentState{SegNo: n, Timescale: 90000, StartPTS: int64(n) * 100, EndPTS: int64(n+1) * 100})
		published[n] = struct{}{}
	}
	rep.MarkPublished(published)
}

func TestSnapshotSortsSegmentsBySegNo(t *testing.T) {
	ci := NewChannelIndex()
	// Reverse order in, ascending order expected out.
	commitPublished(t, ci, "p0", "0", "v0", 5, 4, 3)

	got := ci.Snapshot().Published("p0", "0", "v0")
	if len(got) != 3 {
		t.Fatalf("got %d segments, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].SegNo >= got[i].SegNo {
			t.Fatalf("segments not ascending by SegNo: %v", segNos(got))
		}
	}
}

// The snapshot must be a copy. Committing after it was taken must not change
// what it reports — that is the entire reason it exists.
func TestSnapshotIsDetachedFromLaterWrites(t *testing.T) {
	ci := NewChannelIndex()
	commitPublished(t, ci, "p0", "0", "v0", 1, 2)

	snap := ci.Snapshot()
	before := len(snap.Published("p0", "0", "v0"))

	commitPublished(t, ci, "p0", "0", "v0", 3, 4)

	if after := len(snap.Published("p0", "0", "v0")); after != before {
		t.Errorf("snapshot changed after later commits: %d → %d segments", before, after)
	}
	if live := len(ci.Snapshot().Published("p0", "0", "v0")); live != 4 {
		t.Errorf("a fresh snapshot sees %d segments, want 4", live)
	}
}

// Reordering the slice a snapshot handed out must not corrupt the snapshot for
// the next reader, since generation passes the same snapshot to several
// builders.
func TestSnapshotPublishedIsStableAcrossReaders(t *testing.T) {
	ci := NewChannelIndex()
	commitPublished(t, ci, "p0", "0", "v0", 1, 2, 3)
	snap := ci.Snapshot()

	first := segNos(snap.Published("p0", "0", "v0"))
	second := segNos(snap.Published("p0", "0", "v0"))
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("two reads of the same snapshot differ: %v vs %v", first, second)
		}
	}
}

func TestSnapshotMissingRepIsEmptyNotPanic(t *testing.T) {
	snap := NewChannelIndex().Snapshot()
	if got := snap.Published("nope", "0", "v0"); got != nil {
		t.Errorf("Published for an unknown rep = %v, want nil", got)
	}
	if snap.HasRep("nope", "0", "v0") {
		t.Error("HasRep reported true for an unknown rep")
	}
	if ids := snap.PeriodIDs(); len(ids) != 0 {
		t.Errorf("PeriodIDs on an empty index = %v, want none", ids)
	}
}

// PeriodIDs must be sorted, so a synthesized presentation does not reshuffle
// between requests.
func TestSnapshotPeriodIDsSorted(t *testing.T) {
	ci := NewChannelIndex()
	for _, p := range []string{"2", "0", "1"} {
		commitPublished(t, ci, p, "0", "v0", 1)
	}
	got := ci.Snapshot().PeriodIDs()
	want := []string{"0", "1", "2"}
	if len(got) != len(want) {
		t.Fatalf("PeriodIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PeriodIDs = %v, want %v", got, want)
		}
	}
}

// A representation that exists but has published nothing must not appear as a
// period with content — SynthesizeHLSMPD keys off PeriodIDs to decide which
// periods to emit at all.
func TestSnapshotIncludesRepsWithNoPublishedSegments(t *testing.T) {
	ci := NewChannelIndex()
	ci.Period("p0").AS("0").Rep("v0", 0) // created, nothing committed

	snap := ci.Snapshot()
	if !snap.HasRep("p0", "0", "v0") {
		t.Error("HasRep = false for a rep that exists with nothing published")
	}
	if got := snap.Published("p0", "0", "v0"); len(got) != 0 {
		t.Errorf("Published = %v, want empty", got)
	}
}
