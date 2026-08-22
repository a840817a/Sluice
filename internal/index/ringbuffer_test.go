package index

import (
	"testing"
)

func TestRingBuffer_PushWithinCapacity(t *testing.T) {
	rb := NewRingBuffer(5)
	for i := uint64(1); i <= 3; i++ {
		rb.Push(SegmentState{SegNo: i})
	}
	snap := rb.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(snap))
	}
	for i, s := range snap {
		if s.SegNo != uint64(i+1) {
			t.Errorf("snap[%d].SegNo = %d, want %d", i, s.SegNo, i+1)
		}
	}
}

func TestRingBuffer_Eviction(t *testing.T) {
	rb := NewRingBuffer(3)
	for i := uint64(1); i <= 5; i++ {
		rb.Push(SegmentState{SegNo: i})
	}
	snap := rb.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", len(snap))
	}
	// Oldest remaining should be 3, newest 5.
	if snap[0].SegNo != 3 {
		t.Errorf("snap[0].SegNo = %d, want 3", snap[0].SegNo)
	}
	if snap[2].SegNo != 5 {
		t.Errorf("snap[2].SegNo = %d, want 5", snap[2].SegNo)
	}
}

func TestRingBuffer_UpdateWhere(t *testing.T) {
	rb := NewRingBuffer(4)
	for i := uint64(1); i <= 4; i++ {
		rb.Push(SegmentState{SegNo: i, Status: StatusCommitted})
	}
	rb.UpdateWhere(func(s *SegmentState) {
		if s.SegNo%2 == 0 {
			s.Status = StatusPublished
		}
	})
	for _, s := range rb.Snapshot() {
		if s.SegNo%2 == 0 && s.Status != StatusPublished {
			t.Errorf("SegNo %d: expected Published, got %v", s.SegNo, s.Status)
		}
		if s.SegNo%2 != 0 && s.Status != StatusCommitted {
			t.Errorf("SegNo %d: expected Committed, got %v", s.SegNo, s.Status)
		}
	}
}

func TestRingBuffer_Unbounded(t *testing.T) {
	rb := NewRingBuffer(0) // unbounded
	for i := uint64(1); i <= 500; i++ {
		rb.Push(SegmentState{SegNo: i})
	}
	snap := rb.Snapshot()
	if len(snap) != 500 {
		t.Fatalf("expected 500 entries in unbounded buffer, got %d", len(snap))
	}
	if snap[0].SegNo != 1 || snap[499].SegNo != 500 {
		t.Errorf("unexpected ordering in unbounded buffer")
	}
}
