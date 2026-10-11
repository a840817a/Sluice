package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/queue"
)

// prepareCall is one preparation the gate started. Answer it with done.
type prepareCall struct {
	path string
	task queue.SegmentTask
	done chan error
}

// gateHarness drives an initGate with a fake preparer and a clock the test
// moves, and records what the gate delivered and abandoned. Results come from
// a real Broker, so the test can check that every one was settled.
type gateHarness struct {
	t        *testing.T
	broker   *queue.Broker
	results  chan fetch.Result
	tick     chan time.Time
	prepares chan prepareCall
	cancel   context.CancelFunc
	stopped  chan struct{}

	mu     sync.Mutex
	now    time.Time
	events []string
}

// The broker drops tasks whose deadline has passed on the real clock, so the
// fake clock starts far enough ahead of it that its deadlines are all still
// live when popped.
var gateEpoch = time.Now().Add(time.Hour)

func startGate(t *testing.T, limit int) *gateHarness {
	t.Helper()
	h := &gateHarness{
		t:        t,
		broker:   queue.NewBroker(0),
		results:  make(chan fetch.Result),
		tick:     make(chan time.Time),
		prepares: make(chan prepareCall, 16),
		stopped:  make(chan struct{}),
		now:      gateEpoch,
	}
	g := &initGate{
		limit:    limit,
		initPath: func(task *queue.SegmentTask) string { return task.PeriodID + "/" + task.RepID },
		prepare: func(ctx context.Context, task *queue.SegmentTask, path string) error {
			call := prepareCall{path: path, task: *task, done: make(chan error, 1)}
			h.prepares <- call
			select {
			case err := <-call.done:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		deliver: func(res fetch.Result) { h.record("deliver %d", res.Task.SegNo) },
		abandon: func(res fetch.Result, err error) { h.record("abandon %d: %v", res.Task.SegNo, err) },
		now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		},
		tick: h.tick,
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.stopped)
		g.run(ctx, h.results)
	}()
	t.Cleanup(func() {
		h.stop()
		if !h.broker.Idle() {
			t.Error("a result the gate read was never settled")
		}
	})
	return h
}

func (h *gateHarness) record(format string, segNo uint64, rest ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, fmt.Sprintf(format, append([]any{segNo}, rest...)...))
}

// send hands the gate a fetched result for task, popped from the broker.
func (h *gateHarness) send(task *queue.SegmentTask, fetchErr error) {
	h.t.Helper()
	h.broker.Push(task)
	attempt := h.broker.Pop()
	if attempt == nil {
		h.t.Fatalf("broker did not hand out segment %d", task.SegNo)
	}
	select {
	case h.results <- fetch.Result{Attempt: attempt, Task: attempt.SegmentTask, Err: fetchErr}:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("gate did not read segment %d", task.SegNo)
	}
}

// trySend reports whether the gate reads a result within a short wait.
func (h *gateHarness) trySend(task *queue.SegmentTask) bool {
	h.broker.Push(task)
	attempt := h.broker.Pop()
	select {
	case h.results <- fetch.Result{Attempt: attempt, Task: attempt.SegmentTask}:
		return true
	case <-time.After(100 * time.Millisecond):
		attempt.Finish()
		return false
	}
}

func (h *gateHarness) nextPrepare() prepareCall {
	h.t.Helper()
	select {
	case c := <-h.prepares:
		return c
	case <-time.After(5 * time.Second):
		h.t.Fatal("gate started no preparation")
		return prepareCall{}
	}
}

func (h *gateHarness) noPrepare() {
	h.t.Helper()
	select {
	case c := <-h.prepares:
		h.t.Fatalf("gate started an unexpected preparation of %s for segment %d", c.path, c.task.SegNo)
	case <-time.After(50 * time.Millisecond):
	}
}

func (h *gateHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
	h.tick <- h.now
}

// expect waits until the gate has recorded exactly want, in order.
func (h *gateHarness) expect(want ...string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		got := strings.Join(h.events, "; ")
		h.mu.Unlock()
		if got == strings.Join(want, "; ") {
			return
		}
		if time.Now().After(deadline) || len(got) > len(strings.Join(want, "; ")) {
			h.t.Fatalf("events = %q\nwant     %q", got, strings.Join(want, "; "))
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *gateHarness) stop() {
	h.cancel()
	<-h.stopped
}

func fmp4Task(segNo uint64, deadline time.Duration) *queue.SegmentTask {
	task := videoTask(segNo, queue.FormatFMP4)
	if deadline > 0 {
		task.Deadline = gateEpoch.Add(deadline)
	}
	return task
}

func TestInitGate(t *testing.T) {
	gone := errors.New("init gone")

	t.Run("a fetch error and media without an init pass straight through", func(t *testing.T) {
		h := startGate(t, 2)
		h.send(fmp4Task(1, 0), errors.New("fetch failed"))
		h.send(videoTask(2, queue.FormatTS), nil)
		h.expect("deliver 1", "deliver 2")
		h.noPrepare()
	})

	t.Run("media waits for its init, prepared once per init", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 0), nil)
		h.send(fmp4Task(2, 0), nil)
		call := h.nextPrepare()
		h.noPrepare()
		h.expect()
		call.done <- nil
		h.expect("deliver 1", "deliver 2")
		h.send(fmp4Task(3, 0), nil)
		h.expect("deliver 1", "deliver 2", "deliver 3")
		h.noPrepare()
	})

	t.Run("each period's init is prepared on its own", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 0), nil)
		other := fmp4Task(2, 0)
		other.PeriodID = "1"
		h.send(other, nil)
		first, second := h.nextPrepare(), h.nextPrepare()
		if first.path == second.path {
			t.Fatalf("both preparations are of %s", first.path)
		}
		second.done <- nil
		h.expect("deliver 2")
		first.done <- nil
		h.expect("deliver 2", "deliver 1")
	})

	t.Run("a failed init abandons its media and stays failed for finite work", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 0), nil)
		h.nextPrepare().done <- gone
		h.expect("abandon 1: init gone")
		h.send(fmp4Task(2, 0), nil)
		h.expect("abandon 1: init gone", "abandon 2: init gone")
		h.noPrepare()
	})

	t.Run("later live media renews a failed init, earlier media stays abandoned", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 10*time.Second), nil)
		call := h.nextPrepare()
		h.send(fmp4Task(2, 20*time.Second), nil)
		call.done <- gone
		// Segment 1's deadline is the failed batch's own; segment 2's is later.
		h.expect("abandon 1: init gone")
		renewed := h.nextPrepare()
		if renewed.task.SegNo != 2 {
			t.Fatalf("renewed preparation is for segment %d, want 2", renewed.task.SegNo)
		}
		h.send(fmp4Task(3, 15*time.Second), nil)
		renewed.done <- nil
		h.expect("abandon 1: init gone", "deliver 2", "deliver 3")
	})

	t.Run("live media arriving after a failure renews the init", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 10*time.Second), nil)
		h.nextPrepare().done <- gone
		h.expect("abandon 1: init gone")
		h.send(fmp4Task(2, 20*time.Second), nil)
		h.nextPrepare().done <- nil
		h.expect("abandon 1: init gone", "deliver 2")
	})

	t.Run("one init failing does not hold another's media", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 0), nil)
		other := fmp4Task(2, 0)
		other.RepID = "a0"
		h.send(other, nil)
		calls := map[string]prepareCall{}
		for range 2 {
			c := h.nextPrepare()
			calls[c.path] = c
		}
		calls["0/v0"].done <- gone
		h.expect("abandon 1: init gone")
		calls["0/a0"].done <- nil
		h.expect("abandon 1: init gone", "deliver 2")
	})

	t.Run("media whose deadline passes while waiting is abandoned", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 5*time.Second), nil)
		h.send(fmp4Task(2, 30*time.Second), nil)
		call := h.nextPrepare()
		h.advance(10 * time.Second)
		h.expect("abandon 1: media deadline expired while waiting for init")
		call.done <- nil
		h.expect("abandon 1: media deadline expired while waiting for init", "deliver 2")
	})

	t.Run("a full buffer stops reading media until an init completes", func(t *testing.T) {
		h := startGate(t, 1)
		h.send(fmp4Task(1, 0), nil)
		call := h.nextPrepare()
		if h.trySend(fmp4Task(2, 0)) {
			t.Fatal("gate read media beyond its limit")
		}
		call.done <- nil
		h.expect("deliver 1")
		h.send(fmp4Task(3, 0), nil)
		h.expect("deliver 1", "deliver 3")
	})

	t.Run("stopping settles waiting media and waits for preparations", func(t *testing.T) {
		h := startGate(t, 4)
		h.send(fmp4Task(1, 0), nil)
		h.nextPrepare() // never answered: only cancellation ends it
		h.stop()
		h.expect()
	})
}
