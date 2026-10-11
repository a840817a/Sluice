package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
	"github.com/a840817a/sluice/internal/validate"
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
	path      string
	timescale uint32
	err       error
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
			ts, err := p.prepareInit(ctx, pool, &task, path)
			select {
			case completed <- initCompletion{path: path, timescale: ts, err: err}:
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
			if result.err == nil {
				p.intake.repTimescales[result.path] = result.timescale
			}
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

// prepareInit first reuses a validated local init. Only init is retried; media
// already in memory keeps its retry budget. The deadline belongs to live work,
// while finite work is bounded by a retry budget even for a zero-value config.
func (p *Processor) prepareInit(ctx context.Context, pool *fetch.Pool, task *queue.SegmentTask, path string) (uint32, error) {
	if !task.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, task.Deadline)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if ts, err := validate.ReadInitTimescale(path); err == nil {
		return ts, nil
	}
	// A rebuilt index can point at an ADR-0006 legacy directory. Reuse that
	// init locally, copying it to the encoded location used for new segments.
	if rep := p.ci.FindRep(task.PeriodID, task.ASID, task.RepID); rep != nil {
		for _, seg := range rep.Published() {
			if seg.InitPath == "" || seg.InitPath == path {
				continue
			}
			f, err := os.Open(seg.InitPath)
			if err != nil {
				break
			}
			limit := p.cfg.Worker.MaxSegmentBytes
			if limit <= 0 {
				limit = fetch.DefaultMaxSegmentBytes
			}
			data, err := fetch.ReadAtMost(f, limit)
			_ = f.Close()
			if err == nil && ctx.Err() == nil {
				if ts, err := persistInit(path, data); err == nil {
					return ts, nil
				}
			}
			break
		}
	}
	if task.InitURL == "" {
		return 0, errors.New("fMP4 representation has no usable local init or init URL")
	}
	retries := p.cfg.Worker.MaxRetriesStatic
	if retries <= 0 {
		retries = 5 // Same default as gateway configuration; never retry forever.
	}
	for attempt := 0; ; attempt++ {
		res := pool.FetchInit(ctx, task)
		err := res.Err
		backoff := 2 * time.Second
		if err == nil {
			if err = ctx.Err(); err == nil {
				var ts uint32
				ts, err = persistInit(path, res.InitData)
				if err == nil {
					return ts, nil
				}
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		p.prog.RecordSegmentError(StageInit, err.Error(), task.RepID, task.SegNo)
		if res.StatusCode == http.StatusGone || attempt >= retries {
			return 0, fmt.Errorf("init unavailable after %d attempts: %w", attempt+1, err)
		}
		if res.StatusCode == http.StatusTooEarly {
			backoff = 500 * time.Millisecond
		}
		for i := 0; i < attempt && backoff < time.Minute; i++ {
			backoff *= 2
		}
		timer := time.NewTimer(min(backoff, time.Minute))
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
}

// persistInit returns a timescale only after validation and atomic persistence.
// Both asynchronous preparation and direct Intake callers use this rule.
func persistInit(path string, data []byte) (uint32, error) {
	if err := validate.ValidateInit(data); err != nil {
		return 0, fmt.Errorf("validate init: %w", err)
	}
	ts, err := validate.TimescaleFromInitBytes(data)
	if err != nil {
		return 0, fmt.Errorf("read init timescale: %w", err)
	}
	if err := store.Write(path, data); err != nil {
		return 0, fmt.Errorf("write init: %w", err)
	}
	return ts, nil
}
