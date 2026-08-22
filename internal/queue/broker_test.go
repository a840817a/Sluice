package queue

import (
	"testing"
	"time"
)

func makeTask(segNo uint64, priority int) *SegmentTask {
	return &SegmentTask{
		ChannelID: "ch",
		PeriodID:  "p0",
		ASID:      "a0",
		RepID:     "r0",
		SegNo:     segNo,
		Priority:  priority,
	}
}

func TestBroker_PriorityOrder(t *testing.T) {
	b := NewBroker(0)
	b.Push(makeTask(3, 3))
	b.Push(makeTask(1, 1))
	b.Push(makeTask(2, 2))

	for want := uint64(1); want <= 3; want++ {
		got := b.Pop()
		if got == nil {
			t.Fatalf("Pop returned nil, want SegNo=%d", want)
		}
		if got.SegNo != want {
			t.Errorf("Pop SegNo=%d, want %d", got.SegNo, want)
		}
	}
}

func TestBroker_DuplicateIgnored(t *testing.T) {
	b := NewBroker(0)
	b.Push(makeTask(1, 1))
	b.Push(makeTask(1, 1)) // same key, should be ignored
	if b.Len() != 1 {
		t.Errorf("expected queue length 1 after duplicate push, got %d", b.Len())
	}
	// A deduplicated push loses no work, so it must not count as a drop —
	// otherwise every HLS backfill re-push would look like data loss.
	if got := b.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d after a duplicate push, want 0", got)
	}
}

func TestBroker_ExpiredDropped(t *testing.T) {
	b := NewBroker(0)
	t1 := makeTask(1, 1)
	t1.Deadline = time.Now().Add(-time.Second) // already expired
	b.Push(t1)

	got := b.Pop()
	if got != nil {
		t.Errorf("expected nil from Pop on expired task, got SegNo=%d", got.SegNo)
	}
	if got := b.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d after pushing an expired task, want 1", got)
	}
}

func TestBroker_Requeue_Backoff(t *testing.T) {
	b := NewBroker(0)
	task := makeTask(1, 0)
	b.Push(task)
	task = b.Pop()
	if task == nil {
		t.Fatal("initial Pop returned nil")
	}

	// Requeue with 1s backoff.
	const maxRetries = 3
	const backoff = time.Second
	ok := b.Requeue(task, maxRetries, backoff)
	if !ok {
		t.Fatal("Requeue returned false unexpectedly")
	}

	// Task should not be immediately available.
	got := b.Pop()
	if got != nil {
		t.Errorf("expected nil from Pop during backoff, got SegNo=%d", got.SegNo)
	}
}

func TestBroker_MaxSize_Eviction(t *testing.T) {
	b := NewBroker(2)
	b.Push(makeTask(1, 1)) // high priority (lower number)
	b.Push(makeTask(2, 2))
	// Adding a third, higher-priority task should evict the worst (priority=2).
	b.Push(makeTask(3, 0))

	if b.Len() != 2 {
		t.Fatalf("expected queue length 2 after eviction, got %d", b.Len())
	}

	// Pop order should be: SegNo=3 (priority 0), then SegNo=1 (priority 1).
	first := b.Pop()
	if first == nil || first.SegNo != 3 {
		t.Errorf("first Pop: want SegNo=3, got %v", first)
	}
	second := b.Pop()
	if second == nil || second.SegNo != 1 {
		t.Errorf("second Pop: want SegNo=1, got %v", second)
	}
	// SegNo=2 should have been evicted.
	if b.Pop() != nil {
		t.Error("expected empty queue after eviction scenario")
	}
	if got := b.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d after evicting one task, want 1", got)
	}
}

// TestBroker_DroppedCountsEveryAbandonment enumerates the ways a task can be
// discarded without ever being fetched. The progress UI subtracts nothing and
// explains nothing on its own: this counter is the only thing that distinguishes
// "still downloading" from "stalled at 97% because 40 segments were thrown away".
func TestBroker_DroppedCountsEveryAbandonment(t *testing.T) {
	t.Run("expired on push", func(t *testing.T) {
		b := NewBroker(0)
		task := makeTask(1, 1)
		task.Deadline = time.Now().Add(-time.Second)
		b.Push(task)
		if got := b.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
	})

	t.Run("expired between push and pop", func(t *testing.T) {
		// Valid when queued, stale by the time a worker got to it. Broker.Pop
		// skips it with a bare continue, so nothing else records this loss.
		b := NewBroker(0)
		task := makeTask(1, 1)
		task.Deadline = time.Now().Add(time.Hour)
		b.Push(task)
		if got := b.Dropped(); got != 0 {
			t.Fatalf("Dropped() = %d before expiry, want 0", got)
		}
		task.Deadline = time.Now().Add(-time.Second) // the broker holds this pointer
		if got := b.Pop(); got != nil {
			t.Fatalf("Pop returned SegNo=%d, want nil for an expired task", got.SegNo)
		}
		if got := b.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
	})

	t.Run("evicted to stay under capacity", func(t *testing.T) {
		b := NewBroker(1)
		b.Push(makeTask(1, 5))
		b.Push(makeTask(2, 0)) // more urgent: evicts SegNo 1
		if got := b.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
	})

	t.Run("new task less urgent than everything queued", func(t *testing.T) {
		b := NewBroker(1)
		b.Push(makeTask(1, 0))
		b.Push(makeTask(2, 9)) // rejected outright
		if got := b.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
		if b.Len() != 1 {
			t.Errorf("Len = %d, want 1", b.Len())
		}
	})

	t.Run("out of retries", func(t *testing.T) {
		b := NewBroker(0)
		task := makeTask(1, 0)
		if ok := b.Requeue(task, 1, time.Millisecond); !ok {
			t.Fatal("first Requeue returned false, want true")
		}
		if got := b.Dropped(); got != 0 {
			t.Fatalf("Dropped() = %d after a retry within budget, want 0", got)
		}
		if ok := b.Requeue(task, 1, time.Millisecond); ok {
			t.Fatal("Requeue past maxRetries returned true, want false")
		}
		if got := b.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
	})

	t.Run("requeue of an expired task", func(t *testing.T) {
		b := NewBroker(0)
		task := makeTask(1, 0)
		task.Deadline = time.Now().Add(-time.Second)
		if ok := b.Requeue(task, 5, time.Millisecond); ok {
			t.Fatal("Requeue of an expired task returned true, want false")
		}
		if got := b.Dropped(); got != 1 {
			t.Errorf("Dropped() = %d, want 1", got)
		}
	})

	t.Run("a normal push and pop drops nothing", func(t *testing.T) {
		b := NewBroker(0)
		b.Push(makeTask(1, 0))
		if got := b.Pop(); got == nil {
			t.Fatal("Pop returned nil for a healthy task")
		}
		if got := b.Dropped(); got != 0 {
			t.Errorf("Dropped() = %d on the happy path, want 0", got)
		}
	})
}
