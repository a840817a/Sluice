package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/queue"
)

// initialization belongs to one stored period/representation. A failed attempt
// is terminal for its batch. Later live work may renew it under a later deadline.
type initialization struct {
	done     bool
	err      error
	deadline time.Time
}

func (s *initialization) canRenew(task *queue.SegmentTask) bool {
	return s.done && s.err != nil && !task.Expired() && !task.Deadline.IsZero() && task.Deadline.After(s.deadline)
}

type initCompletion struct {
	path string
	err  error
}

// runWithInit owns the handover from fetched media to Intake. Waiting media
// stays outstanding in the broker and never reaches disk. At most one result
// per configured worker waits here; the pool also has its bounded result
// channel and one body per worker. Each body is bounded by MaxSegmentBytes.
// When this buffer fills we stop reading media, but keep reading init outcomes.
// Init work never uses the media workers or their result channel.
func (p *Processor) runWithInit(ctx context.Context, pool *fetch.Pool) {
	ctx, cancel := context.WithCancel(ctx)
	limit := max(1, p.cfg.Worker.FetchWorkers)
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
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	finish := func(res fetch.Result, err error) {
		if res.Task.Expired() {
			err = errors.New("media deadline expired while waiting for init")
		}
		if err != nil {
			p.prog.RecordSegmentError(StageInit, err.Error(), res.Task.RepID, res.Task.SegNo)
			slog.Warn("segment abandoned: init is not ready", "rep", res.Task.RepID, "seg", res.Task.SegNo, "err", err)
		} else {
			p.handle(res)
		}
		res.Attempt.Finish()
	}
	flush := func() {
		kept := pending[:0]
		for _, res := range pending {
			state := states[p.intake.location(res.Task).InitPath()]
			if (state.done && !state.canRenew(res.Task)) || res.Task.Expired() {
				finish(res, state.err)
			} else {
				kept = append(kept, res)
			}
		}
		clear(pending[len(kept):])
		pending = kept
	}

	startInit := func(path string, source *queue.SegmentTask) *initialization {
		state := &initialization{deadline: source.Deadline}
		states[path] = state
		// The task may later be requeued/mutated. Init owns a copy.
		task := *source
		active++
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.inits.prepare(ctx, &task, path)
			select {
			case completed <- initCompletion{path: path, err: err}:
			case <-ctx.Done():
			}
		}()
		return state
	}

	for {
		results := pool.Results()
		// The active cap also covers jobs whose last waiting media expired
		// just before their cancelled HTTP request returned.
		if len(pending) >= limit || active >= limit {
			results = nil
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
			var next *queue.SegmentTask
			for _, res := range pending {
				if p.intake.location(res.Task).InitPath() == result.path && state.canRenew(res.Task) && (next == nil || res.Task.Deadline.After(next.Deadline)) {
					next = res.Task
				}
			}
			if next != nil {
				startInit(result.path, next)
			}
		case <-ticker.C:
			flush()
		case res, ok := <-results:
			if !ok {
				return
			}
			if res.Err != nil || res.Task.Format != queue.FormatFMP4 {
				p.handle(res)
				res.Attempt.Finish()
				continue
			}
			path := p.intake.location(res.Task).InitPath()
			state := states[path]
			if state == nil || state.canRenew(res.Task) {
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
