package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/queue"
)

// initialization belongs to one stored period/representation. A failed attempt
// is terminal for its batch. Later live work may renew it under a later
// deadline, and any work may once keys were added after it started.
type initialization struct {
	done     bool
	err      error
	deadline time.Time
	// keyEpoch is the gate's keyEpoch when the attempt started.
	keyEpoch uint64
}

func (s *initialization) canRenew(task *queue.SegmentTask, now time.Time, keyEpoch uint64) bool {
	if !s.done || s.err == nil || task.ExpiredAt(now) {
		return false
	}
	return keyEpoch != s.keyEpoch || (!task.Deadline.IsZero() && task.Deadline.After(s.deadline))
}

type initCompletion struct {
	path string
	err  error
}

// initGate owns the handover from fetched media to Intake: no fMP4 segment is
// delivered before its representation's init is usable. Waiting media stays
// outstanding in the broker and never reaches disk. At most limit results wait
// here; the pool also has its bounded result channel and one body per worker,
// each bounded by MaxSegmentBytes. When the buffer fills the gate stops
// reading media, but keeps reading init outcomes. Init work never uses the
// media workers or their result channel.
//
// One init preparation runs per init path at a time. A failure is final for
// the media waiting on it, unless later live media carries a later deadline,
// or keys were added since the failed preparation started (an init refused
// for want of a key): then preparation starts again for the media that can
// still use it.
//
// The gate settles every result it reads: after deliver or abandon, and for
// whatever still waits when run returns.
type initGate struct {
	// limit caps both the media waiting and the preparations running.
	limit int
	// initPath names where task's init lives; it identifies the init.
	initPath func(task *queue.SegmentTask) string
	// prepare makes the init at path usable; it runs on a goroutine of its own.
	prepare func(ctx context.Context, task *queue.SegmentTask, path string) error
	// deliver hands over a result that may be stored: a failed fetch, a format
	// without an init, or media whose init is usable.
	deliver func(fetch.Result)
	// abandon drops media whose init will not become usable.
	abandon func(fetch.Result, error)
	// now and tick are the gate's clock; tick prompts it to drop waiting media
	// whose deadline has passed.
	now  func() time.Time
	tick <-chan time.Time
	// exclusive delivers work to run between deliveries (Processor.Exclusive).
	exclusive <-chan exclusiveWork
	// keyEpoch changes each time content keys are added; nil means never.
	keyEpoch func() uint64
}

// exclusiveWork is a function to run on the gate's goroutine, and where its
// result goes.
type exclusiveWork struct {
	fn   func() error
	done chan<- error
}

// run reads results until they close or ctx ends, and returns only after
// every preparation it started has returned.
func (g *initGate) run(ctx context.Context, results <-chan fetch.Result) {
	ctx, cancel := context.WithCancel(ctx)
	limit := max(1, g.limit)
	completed := make(chan initCompletion, limit)
	states := make(map[string]*initialization)
	var pending []fetch.Result
	var wg sync.WaitGroup
	active := 0
	defer func() {
		cancel()
		wg.Wait() // Stop/Join must include every init fetch and write.
		for _, res := range pending {
			res.Attempt.Finish()
		}
	}()

	epoch := func() uint64 {
		if g.keyEpoch == nil {
			return 0
		}
		return g.keyEpoch()
	}
	finish := func(res fetch.Result, err error) {
		if res.Task.ExpiredAt(g.now()) {
			err = errors.New("media deadline expired while waiting for init")
		}
		if err != nil {
			g.abandon(res, err)
		} else {
			g.deliver(res)
		}
		res.Attempt.Finish()
	}
	flush := func() {
		now := g.now()
		kept := pending[:0]
		for _, res := range pending {
			state := states[g.initPath(res.Task)]
			if (state.done && !state.canRenew(res.Task, now, epoch())) || res.Task.ExpiredAt(now) {
				finish(res, state.err)
			} else {
				kept = append(kept, res)
			}
		}
		clear(pending[len(kept):])
		pending = kept
	}

	startInit := func(path string, source *queue.SegmentTask) *initialization {
		state := &initialization{deadline: source.Deadline, keyEpoch: epoch()}
		states[path] = state
		// The task may later be requeued/mutated. Init owns a copy.
		task := *source
		active++
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.prepare(ctx, &task, path)
			select {
			case completed <- initCompletion{path: path, err: err}:
			case <-ctx.Done():
			}
		}()
		return state
	}

	for {
		in := results
		// The active cap also covers jobs whose last waiting media expired
		// just before their cancelled HTTP request returned.
		if len(pending) >= limit || active >= limit {
			in = nil
		}
		select {
		case <-ctx.Done():
			return
		case result := <-completed:
			active--
			state := states[result.path]
			state.done, state.err = true, result.err
			flush()
			// Preserve fetched media with later live deadlines. Use the latest
			// pending deadline so a failed batch cannot restart once per segment.
			now, ep := g.now(), epoch()
			var next *queue.SegmentTask
			for _, res := range pending {
				if g.initPath(res.Task) == result.path && state.canRenew(res.Task, now, ep) && (next == nil || res.Task.Deadline.After(next.Deadline)) {
					next = res.Task
				}
			}
			if next != nil {
				startInit(result.path, next)
			}
		case <-g.tick:
			flush()
		case work := <-g.exclusive:
			if ctx.Err() != nil {
				work.done <- ErrStopped
				continue
			}
			work.done <- work.fn()
		case res, ok := <-in:
			if !ok {
				return
			}
			if res.Err != nil || res.Task.Format != queue.FormatFMP4 {
				g.deliver(res)
				res.Attempt.Finish()
				continue
			}
			path := g.initPath(res.Task)
			state := states[path]
			if state == nil || state.canRenew(res.Task, g.now(), epoch()) {
				state = startInit(path, res.Task)
			}
			if state.done {
				finish(res, state.err)
			} else {
				pending = append(pending, res)
			}
		}
	}
}
