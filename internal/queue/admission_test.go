package queue

import (
	"context"
	"testing"
	"time"
)

func TestPreservingAdmissionConcurrentProducersAndRetries(t *testing.T) {
	const producers, each, capacity = 4, 16, 2
	broker := NewBroker(capacity)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan bool, producers)
	for producer := range producers {
		go func() {
			for i := range each {
				if !broker.PushPreserving(ctx, makeTask(uint64(producer*each+i), i)) {
					finished <- false
					return
				}
			}
			finished <- true
		}()
	}
	stored := make(map[uint64]bool)
	for len(stored) < producers*each {
		if ctx.Err() != nil {
			t.Fatalf("only stored %d/%d tasks; dropped=%d", len(stored), producers*each, broker.Dropped())
		}
		task := broker.Pop()
		if task == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		if task.Retries == 0 {
			if !task.Retry(1, time.Millisecond) {
				t.Fatal("first attempt could not be retried")
			}
		} else {
			if stored[task.SegNo] {
				t.Fatalf("task %d completed twice", task.SegNo)
			}
			stored[task.SegNo] = true
		}
		task.Finish()
		// This test has one consumer: only its popped task can add retry
		// overflow. Repeated failures must not accumulate unbounded work.
		if got := broker.Len(); got > capacity+1 {
			t.Fatalf("pending queue grew to %d with only one task in flight", got)
		}
	}
	for range producers {
		select {
		case ok := <-finished:
			if !ok {
				t.Fatal("producer canceled before admitting all work")
			}
		case <-ctx.Done():
			t.Fatal("producer did not finish")
		}
	}
	if broker.Dropped() != 0 || !broker.Idle() {
		t.Fatalf("finished asset: dropped=%d idle=%v", broker.Dropped(), broker.Idle())
	}
}

func TestPreservingAdmissionWaitsForRoomAndCancels(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		name := "room becomes available"
		if cancelWaiting {
			name = "cancel while full"
		}
		t.Run(name, func(t *testing.T) {
			broker := NewBroker(1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			first, second := makeTask(1, 1), makeTask(2, 2)
			if !broker.PushPreserving(ctx, first) {
				t.Fatal("first admission failed")
			}
			started, done := make(chan struct{}), make(chan bool, 1)
			go func() {
				close(started)
				done <- broker.PushPreserving(ctx, second)
			}()
			<-started
			select {
			case <-done:
				t.Fatal("admission returned while the queue was full")
			case <-time.After(20 * time.Millisecond):
			}
			if cancelWaiting {
				cancel()
			} else {
				task := broker.Pop()
				if task == nil || task.SegmentTask != first {
					t.Fatal("original task was displaced")
				}
				task.Finish()
			}
			select {
			case admitted := <-done:
				if admitted == cancelWaiting {
					t.Fatalf("admitted=%v, canceled=%v", admitted, cancelWaiting)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked admission did not resume")
			}
			want := second
			if cancelWaiting {
				want = first
			}
			task := broker.Pop()
			if task == nil || task.SegmentTask != want || broker.Dropped() != 0 {
				t.Fatal("waiting or cancellation lost the queued task")
			}
			task.Finish()
			if !broker.Idle() {
				t.Fatal("unexpected work after drain")
			}
		})
	}
}

func TestPreservingAdmissionAlreadyCanceled(t *testing.T) {
	for _, capacity := range []int{0, 1} {
		broker := NewBroker(capacity)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if broker.PushPreserving(ctx, makeTask(1, 1)) {
			t.Fatal("already-canceled source admitted new work")
		}
		if !broker.Idle() || broker.Dropped() != 0 {
			t.Fatal("cancellation changed queue or drop accounting")
		}
	}
}

func TestPreservingAdmissionWaitsBehindLiveWork(t *testing.T) {
	broker := NewBroker(1)
	live := makeTask(1, 9)
	broker.Push(live)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if broker.PushPreserving(ctx, makeTask(2, 0)) {
		t.Fatal("finite admission displaced live work instead of waiting for room")
	}
	task := broker.Pop()
	if task == nil || task.SegmentTask != live || broker.Dropped() != 0 {
		t.Fatal("waiting finite admission lost existing live work")
	}
	task.Finish()
}

func TestLiveRetryStillHonorsCapacityAndPriority(t *testing.T) {
	for _, queuedPriority := range []int{0, 9} {
		broker := NewBroker(1)
		retry := makeTask(1, 0)
		broker.Push(retry)
		task := broker.Pop()
		queued := makeTask(2, queuedPriority)
		broker.Push(queued)
		task.Retry(1, 0)
		task.Finish()
		if broker.Len() != 1 || broker.Dropped() != 1 {
			t.Fatalf("live retry bypassed capacity: len=%d dropped=%d", broker.Len(), broker.Dropped())
		}
		want := queued
		if queuedPriority == 9 {
			want = retry
		}
		task = broker.Pop()
		if task == nil || task.SegmentTask != want {
			t.Fatal("live retry changed priority eviction")
		}
		task.Finish()
	}
}

func TestLiveAdmissionCannotEvictPreservedWork(t *testing.T) {
	t.Run("mixed queue evicts only live work", func(t *testing.T) {
		broker := NewBroker(2)
		finite, live, urgent := makeTask(1, 9), makeTask(2, 5), makeTask(3, 0)
		broker.PushPreserving(context.Background(), finite)
		broker.Push(live)
		broker.Push(urgent)
		task, other := broker.Pop(), broker.Pop()
		if task == nil || other == nil || task.SegmentTask != urgent || other.SegmentTask != finite || broker.Dropped() != 1 {
			t.Fatal("live admission did not preserve finite work and evict the live candidate")
		}
		task.Finish()
		other.Finish()
	})
	t.Run("all protected rejects incoming live work", func(t *testing.T) {
		broker := NewBroker(1)
		finite := makeTask(1, 9)
		broker.PushPreserving(context.Background(), finite)
		broker.Push(makeTask(2, 0))
		task := broker.Pop()
		if task == nil || task.SegmentTask != finite || broker.Dropped() != 1 {
			t.Fatal("live work evicted a protected task")
		}
		task.Finish()
	})
}

func TestPreservingAdmissionPromotesQueuedDuplicate(t *testing.T) {
	broker := NewBroker(1)
	original := makeTask(1, 9)
	broker.Push(original)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !broker.PushPreserving(ctx, makeTask(1, 9)) {
		t.Fatal("queued duplicate waited for capacity instead of being preserved")
	}
	broker.Push(makeTask(2, 0))
	task := broker.Pop()
	if task == nil || task.SegmentTask != original || broker.Dropped() != 1 {
		t.Fatal("duplicate did not preserve the original queued task")
	}
	task.Finish()
}

func TestPreservingAdmissionPromotionTransfersDeadlinePolicy(t *testing.T) {
	t.Run("finite source clears stale live deadline", func(t *testing.T) {
		broker := NewBroker(1)
		live := makeTask(1, 9)
		live.Deadline = time.Now().Add(time.Hour)
		broker.Push(live)

		// The broker stores this pointer, so making its old live deadline
		// expired is deterministic and does not require waiting for a clock.
		live.Deadline = time.Now().Add(-time.Second)
		finite := makeTask(1, 9) // zero is the normal finite-source deadline
		if !broker.PushPreserving(context.Background(), finite) {
			t.Fatal("finite duplicate was not admitted")
		}
		if !live.Deadline.IsZero() {
			t.Fatal("finite source did not clear the stale live deadline")
		}
		task := broker.Pop()
		if task == nil || task.SegmentTask != live || broker.Dropped() != 0 {
			t.Fatal("promoted finite task was dropped by its old live deadline")
		}
		task.Finish()
	})

	t.Run("explicit finite expiry remains effective", func(t *testing.T) {
		broker := NewBroker(1)
		live := makeTask(1, 9)
		live.Deadline = time.Now().Add(time.Hour)
		broker.Push(live)
		finite := makeTask(1, 9)
		finite.Deadline = time.Now().Add(-time.Second)
		if !broker.PushPreserving(context.Background(), finite) {
			t.Fatal("finite duplicate was not admitted")
		}
		if broker.Pop() != nil || broker.Dropped() != 1 {
			t.Fatal("explicit finite expiry was lost during promotion")
		}
	})
}

func TestPreservedRequeueCollisionTransfersDeadlinePolicy(t *testing.T) {
	broker := NewBroker(1)
	finite := makeTask(1, 9)
	if !broker.PushPreserving(context.Background(), finite) {
		t.Fatal("finite task was not admitted")
	}
	attempt := broker.Pop()
	if attempt == nil || attempt.SegmentTask != finite {
		t.Fatal("could not fetch finite task")
	}

	live := makeTask(1, 9)
	live.Deadline = time.Now().Add(time.Hour)
	broker.Push(live)
	// As above, expire the live copy without relying on test scheduling.
	live.Deadline = time.Now().Add(-time.Second)
	if !attempt.Retry(1, 0) {
		t.Fatal("finite retry was not requeued")
	}
	attempt.Finish()

	if !live.preserve || !live.Deadline.IsZero() {
		t.Fatal("requeue collision did not transfer finite deadline policy")
	}
	task := broker.Pop()
	if task == nil || task.SegmentTask != live || broker.Dropped() != 0 {
		t.Fatal("requeue collision left the queued live deadline in effect")
	}
	task.Finish()
}

func TestPreservingAdmissionStillDropsExpiredWork(t *testing.T) {
	broker := NewBroker(1)
	task := makeTask(1, 1)
	task.Deadline = time.Now().Add(-time.Second)
	broker.PushPreserving(context.Background(), task)
	if !broker.Idle() || broker.Dropped() != 1 {
		t.Fatal("preservation bypassed expiry")
	}
}
