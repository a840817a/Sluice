package queue

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Broker is a thread-safe priority queue for SegmentTasks.
// Lower Priority value = dequeued first.
// Expired tasks are silently dropped on pop.
type Broker struct {
	mu      sync.Mutex
	pq      taskHeap
	seen    map[string]struct{} // deduplicate by task key
	maxSize int

	// dropped counts tasks abandoned without ever being fetched. Without it, a
	// channel that sheds load leaves its progress short of the expected total
	// with nothing to explain the gap. It is an atomic rather than a mu-guarded
	// field because Requeue abandons tasks without holding mu.
	dropped atomic.Uint64

	// outstanding counts tasks that have left the queue and not yet been
	// released: being fetched, waiting in the pool's result channel, or being
	// written to disk. It is guarded by mu rather than atomic precisely so that
	// leaving the queue and becoming outstanding happen together — a reader
	// between the two would see no work anywhere and conclude, wrongly, that
	// ingest had finished.
	outstanding int
}

// NewBroker creates a Broker with the given pending-work capacity limit.
// Push sheds live work when full; PushPreserving waits instead. Protected
// retries may temporarily exceed this limit by already in-flight work.
func NewBroker(maxSize int) *Broker {
	return &Broker{
		seen:    make(map[string]struct{}),
		maxSize: maxSize,
	}
}

// Push enqueues a live task. Duplicate keys are ignored. If the queue is over
// capacity, the lowest-priority live task is evicted.
func (b *Broker) Push(t *SegmentTask) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := t.Key()
	if _, ok := b.seen[key]; ok {
		// Not a drop: the same work is already queued, so nothing is lost.
		return
	}

	// Drop expired tasks before checking capacity
	if t.Expired() {
		b.dropped.Add(1)
		return
	}

	// Evict the lowest-priority live entry if over capacity. Completed-source
	// tasks are protected from shedding; a live source may be dropped instead.
	if b.maxSize > 0 && len(b.pq) >= b.maxSize {
		worst := b.pq.worstLiveIndex()
		if worst >= 0 && b.pq[worst].Priority >= t.Priority {
			evicted := b.pq[worst]
			heap.Remove(&b.pq, worst)
			delete(b.seen, evicted.Key())
			b.dropped.Add(1)
		} else {
			b.dropped.Add(1)
			return // new task is worse than everything in queue, drop it
		}
	}

	b.seen[key] = struct{}{}
	heap.Push(&b.pq, t)
}

// PushPreserving admits a task from a finite source. It atomically performs
// duplicate detection, expiry checking, capacity handling, and insertion. It
// waits while the pending queue is full, regardless of which tasks occupy it.
// It returns false only when ctx is cancelled; an expired task is accounted as
// dropped and returns true so a finite producer can continue enumerating.
func (b *Broker) PushPreserving(ctx context.Context, t *SegmentTask) bool {
	for {
		b.mu.Lock()
		if ctx.Err() != nil {
			b.mu.Unlock()
			return false
		}
		key := t.Key()
		if _, ok := b.seen[key]; ok {
			// Upgrade the already queued copy when a finite source rediscovers
			// work previously admitted as live.
			for _, queued := range b.pq {
				if queued.Key() == key {
					queued.preserve = true
					break
				}
			}
			b.mu.Unlock()
			return true
		}
		if t.Expired() {
			b.dropped.Add(1)
			b.mu.Unlock()
			return true
		}
		if b.maxSize <= 0 || len(b.pq) < b.maxSize {
			t.preserve = true
			b.seen[key] = struct{}{}
			heap.Push(&b.pq, t)
			b.mu.Unlock()
			return true
		}
		b.mu.Unlock()

		timer := time.NewTimer(awaitRoomPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		case <-timer.C:
		}
	}
}

// Pop removes and returns the highest-priority non-expired task that is ready
// to execute (not in backoff). Tasks still in their backoff window are skipped
// and re-queued so that lower-priority but immediately runnable tasks are not
// starved. Returns nil if no ready task exists.
func (b *Broker) Pop() *SegmentTask {
	b.mu.Lock()
	defer b.mu.Unlock()

	var deferred []*SegmentTask
	var result *SegmentTask

	for len(b.pq) > 0 {
		t := heap.Pop(&b.pq).(*SegmentTask)
		delete(b.seen, t.Key())

		if t.Expired() {
			b.dropped.Add(1)
			continue
		}
		if !t.NextRetry.IsZero() && time.Now().Before(t.NextRetry) {
			// Not ready yet — collect and re-push after the scan.
			deferred = append(deferred, t)
			continue
		}
		result = t
		b.outstanding++
		break
	}

	// Re-enqueue deferred backoff tasks.
	for _, t := range deferred {
		heap.Push(&b.pq, t)
		b.seen[t.Key()] = struct{}{}
	}

	return result
}

// Release reports that one popped task has reached a terminal state, whatever
// that state was: stored, dropped, or handed back to Requeue. The processor
// calls it once per result after handling, so a task that was requeued is back
// in the queue before its in-flight count drops and the broker never looks
// empty in between.
func (b *Broker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.outstanding > 0 {
		b.outstanding--
	}
}

// Idle reports that this broker has no work anywhere: nothing queued and
// nothing in flight.
//
// This is the question to ask before treating ingest as finished. Len alone
// answers a narrower one — "is the queue empty" — which becomes true the moment
// the last task is popped, while it is still being downloaded.
func (b *Broker) Idle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pq) == 0 && b.outstanding == 0
}

// Len returns the current queue length (including possibly-expired entries).
func (b *Broker) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pq)
}

// awaitRoomPollInterval is how often PushPreserving re-checks for queue room.
// It only polls while the broker is full.
const awaitRoomPollInterval = 5 * time.Millisecond

// Dropped returns the cumulative number of tasks abandoned without being
// fetched — expired, evicted to stay under capacity, or out of retries.
// Deduplicated pushes are not counted: no work is lost when the same segment is
// already queued.
func (b *Broker) Dropped() uint64 { return b.dropped.Load() }

// Requeue pushes a task back after a failed attempt, applying backoff.
// If maxRetries is reached or the task is expired, it is discarded.
func (b *Broker) Requeue(t *SegmentTask, maxRetries int, backoff time.Duration) bool {
	t.Retries++
	if maxRetries > 0 && t.Retries > maxRetries {
		b.dropped.Add(1)
		return false
	}
	if t.Expired() {
		b.dropped.Add(1)
		return false
	}
	// Exponential backoff: backoff * 2^(retries-1)
	wait := backoff
	for i := 1; i < t.Retries; i++ {
		wait *= 2
	}
	t.NextRetry = time.Now().Add(wait)
	// Lower urgency on retry
	t.Priority++
	// Only protected work may bypass capacity. Live retries retain Push's
	// shedding semantics after releasing the lock.
	b.mu.Lock()
	if !t.preserve {
		b.mu.Unlock()
		b.Push(t)
		return true
	}
	// The processor is the sole result consumer: waiting here for capacity
	// could stall the fetch workers behind its full result channel. This task
	// was already admitted, so move it back to pending without shedding. New
	// preserving admissions wait while pending >= maxSize; overflow is bounded
	// by the work already in flight, not the size of the asset.
	if _, exists := b.seen[t.Key()]; !exists {
		b.seen[t.Key()] = struct{}{}
		heap.Push(&b.pq, t)
	} else {
		for _, queued := range b.pq {
			if queued.Key() == t.Key() {
				queued.preserve = true
				break
			}
		}
	}
	b.mu.Unlock()
	return true
}

// ── heap implementation ──────────────────────────────────────────────────────

type taskHeap []*SegmentTask

func (h taskHeap) Len() int           { return len(h) }
func (h taskHeap) Less(i, j int) bool { return h[i].Priority < h[j].Priority }
func (h taskHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *taskHeap) Push(x any) {
	*h = append(*h, x.(*SegmentTask))
}

func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return t
}

func (h taskHeap) worstLiveIndex() int {
	worst := -1
	for i, t := range h {
		if t.preserve {
			continue
		}
		if worst < 0 || t.Priority > h[worst].Priority {
			worst = i
		}
	}
	return worst
}
