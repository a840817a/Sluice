package queue

import (
	"container/heap"
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
}

// NewBroker creates a Broker with the given capacity limit.
// When full, the lowest-priority (highest number) pending task is dropped.
func NewBroker(maxSize int) *Broker {
	return &Broker{
		seen:    make(map[string]struct{}),
		maxSize: maxSize,
	}
}

// Push enqueues a task. Duplicate keys are ignored.
// If the queue is over capacity, the task with lowest urgency is evicted.
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

	// Evict lowest-priority entry if over capacity
	if b.maxSize > 0 && len(b.pq) >= b.maxSize {
		worst := b.pq.worstIndex()
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
		break
	}

	// Re-enqueue deferred backoff tasks.
	for _, t := range deferred {
		heap.Push(&b.pq, t)
		b.seen[t.Key()] = struct{}{}
	}

	return result
}

// Len returns the current queue length (including possibly-expired entries).
func (b *Broker) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pq)
}

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
	b.Push(t)
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

// worstIndex returns the index of the highest Priority (least urgent) entry.
func (h taskHeap) worstIndex() int {
	if len(h) == 0 {
		return -1
	}
	worst := 0
	for i := 1; i < len(h); i++ {
		if h[i].Priority > h[worst].Priority {
			worst = i
		}
	}
	return worst
}
