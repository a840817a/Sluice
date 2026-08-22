package index

import "testing"

// A segment can legitimately be fetched twice: the HLS backfill path re-pushes a
// segment that is still in flight (it has not committed yet, so it looks like a
// hole), and Broker.Pop has already dropped the task key from its dedup set. Two
// entries for one segment number silently truncate the output playlist, so the
// index is the choke point that must collapse them.
func TestRingBufferPushReplacesSameSegNo(t *testing.T) {
	t.Run("fixed capacity", func(t *testing.T) {
		rb := NewRingBuffer(8)
		rb.Push(SegmentState{SegNo: 7, Path: "first", Status: StatusCommitted})
		rb.Push(SegmentState{SegNo: 7, Path: "second", Status: StatusCommitted})

		if rb.Len() != 1 {
			t.Fatalf("Len = %d after pushing SegNo 7 twice, want 1", rb.Len())
		}
		got := rb.Snapshot()
		if len(got) != 1 || got[0].Path != "second" {
			t.Errorf("Snapshot = %+v, want a single entry with the later value", got)
		}
	})

	t.Run("unbounded", func(t *testing.T) {
		rb := NewRingBuffer(0)
		rb.Push(SegmentState{SegNo: 3, Path: "first"})
		rb.Push(SegmentState{SegNo: 3, Path: "second"})

		if rb.Len() != 1 {
			t.Fatalf("Len = %d, want 1", rb.Len())
		}
		if got := rb.Snapshot(); len(got) != 1 || got[0].Path != "second" {
			t.Errorf("Snapshot = %+v, want a single entry with the later value", got)
		}
	})

	t.Run("insertion order survives a replacement", func(t *testing.T) {
		rb := NewRingBuffer(8)
		for _, n := range []uint64{5, 6, 7, 8} {
			rb.Push(SegmentState{SegNo: n})
		}
		rb.Push(SegmentState{SegNo: 6, Path: "updated"})

		got := rb.Snapshot()
		want := []uint64{5, 6, 7, 8}
		if len(got) != len(want) {
			t.Fatalf("Snapshot len = %d, want %d: %+v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i].SegNo != w {
				t.Errorf("Snapshot[%d].SegNo = %d, want %d", i, got[i].SegNo, w)
			}
		}
		if got[1].Path != "updated" {
			t.Errorf("replacement did not land: %+v", got[1])
		}
	})

	t.Run("an evicted segment number can be inserted again", func(t *testing.T) {
		rb := NewRingBuffer(3)
		rb.Push(SegmentState{SegNo: 1})
		rb.Push(SegmentState{SegNo: 2})
		rb.Push(SegmentState{SegNo: 3})
		rb.Push(SegmentState{SegNo: 4}) // evicts 1

		rb.Push(SegmentState{SegNo: 1, Path: "late arrival"}) // evicts 2

		got := rb.Snapshot()
		if len(got) != 3 {
			t.Fatalf("Snapshot len = %d, want 3: %+v", len(got), got)
		}
		var found bool
		for _, s := range got {
			if s.SegNo == 1 && s.Path == "late arrival" {
				found = true
			}
		}
		if !found {
			t.Errorf("re-inserted segment 1 missing after eviction: %+v", got)
		}
	})
}

// Commit is the path every fetched segment takes, so the collapse must be fixed
// there and not only in the buffer primitive.
func TestCommitIsIdempotentPerSegNo(t *testing.T) {
	ci := NewChannelIndex()
	rep := ci.Period("p0").AS("0").Rep("v0", 16)

	for _, n := range []uint64{5, 6, 7} {
		rep.Commit(SegmentState{SegNo: n, StartPTS: int64(n) * 90000, EndPTS: int64(n+1) * 90000, Timescale: 90000})
	}
	// Segment 7 arrives a second time, as a slow fetch racing the next poll.
	rep.Commit(SegmentState{SegNo: 7, StartPTS: 7 * 90000, EndPTS: 8 * 90000, Timescale: 90000})
	rep.Commit(SegmentState{SegNo: 8, StartPTS: 8 * 90000, EndPTS: 9 * 90000, Timescale: 90000})

	got := rep.Committed()
	if len(got) != 4 {
		t.Fatalf("Committed() = %d segments, want 4 (5,6,7,8): %+v", len(got), segNos(got))
	}
	seen := map[uint64]bool{}
	for _, s := range got {
		if seen[s.SegNo] {
			t.Errorf("duplicate SegNo %d in the index", s.SegNo)
		}
		seen[s.SegNo] = true
	}
}

// TestPushReportsWhetherSegmentWasNew pins the signal the ingest progress
// counters depend on. Collapsing a duplicate is invisible in Len() alone once the
// buffer is full, so without this boolean a re-fetched segment would be counted
// as fresh progress and a download bar could exceed 100%.
func TestPushReportsWhetherSegmentWasNew(t *testing.T) {
	t.Run("fixed capacity", func(t *testing.T) {
		rb := NewRingBuffer(8)
		if !rb.Push(SegmentState{SegNo: 7, Path: "first"}) {
			t.Error("Push of a new SegNo returned false")
		}
		if rb.Push(SegmentState{SegNo: 7, Path: "second"}) {
			t.Error("Push of an already-tracked SegNo returned true")
		}
	})

	t.Run("unbounded", func(t *testing.T) {
		rb := NewRingBuffer(0)
		if !rb.Push(SegmentState{SegNo: 3}) {
			t.Error("Push of a new SegNo returned false")
		}
		if rb.Push(SegmentState{SegNo: 3}) {
			t.Error("Push of an already-tracked SegNo returned true")
		}
	})

	t.Run("a full buffer still reports new segments as new", func(t *testing.T) {
		// The case Len() cannot distinguish: at capacity, both a genuinely new
		// segment and a replacement leave Len() unchanged.
		rb := NewRingBuffer(3)
		for _, n := range []uint64{1, 2, 3} {
			rb.Push(SegmentState{SegNo: n})
		}
		if !rb.Push(SegmentState{SegNo: 4}) { // evicts 1, but 4 is new
			t.Error("Push of new SegNo 4 into a full buffer returned false")
		}
		if rb.Len() != 3 {
			t.Fatalf("Len = %d, want 3", rb.Len())
		}
		if rb.Push(SegmentState{SegNo: 4, Path: "again"}) {
			t.Error("re-Push of SegNo 4 returned true")
		}
	})

	t.Run("an evicted segment number counts as new again", func(t *testing.T) {
		rb := NewRingBuffer(3)
		for _, n := range []uint64{1, 2, 3, 4} { // 4 evicts 1
			rb.Push(SegmentState{SegNo: n})
		}
		if !rb.Push(SegmentState{SegNo: 1, Path: "late arrival"}) {
			t.Error("Push of an evicted-then-refetched SegNo returned false")
		}
	})
}

// TestCommitReportsWhetherSegmentWasNew is the same guarantee at the level the
// processor actually calls.
func TestCommitReportsWhetherSegmentWasNew(t *testing.T) {
	ci := NewChannelIndex()
	rep := ci.Period("p0").AS("0").Rep("v0", 16)

	seg := func(n uint64) SegmentState {
		return SegmentState{SegNo: n, StartPTS: int64(n) * 90000, EndPTS: int64(n+1) * 90000, Timescale: 90000}
	}
	if !rep.Commit(seg(5)) {
		t.Error("Commit of a new segment returned false")
	}
	if rep.Commit(seg(5)) {
		t.Error("Commit of an already-committed segment returned true")
	}
	if !rep.Commit(seg(6)) {
		t.Error("Commit of a new segment returned false after a duplicate")
	}

	// The count the progress counters would arrive at must match reality.
	if got := rep.Total(); got != 2 {
		t.Errorf("Total = %d, want 2", got)
	}
}

func segNos(segs []SegmentState) []uint64 {
	out := make([]uint64, len(segs))
	for i, s := range segs {
		out[i] = s.SegNo
	}
	return out
}
