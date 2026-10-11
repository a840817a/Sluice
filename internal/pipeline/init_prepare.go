package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
	"github.com/a840817a/sluice/internal/validate"
)

// initDownloader fetches a representation's init segment from the origin.
// fetch.Fetcher is the production one.
type initDownloader interface {
	FetchInit(ctx context.Context, task *queue.SegmentTask) fetch.Result
}

// initPreparer makes a representation's init segment usable on disk and
// knows the track timescale of every init it has made usable. No fMP4 media
// reaches its final path until its init is usable: validated, persisted, and
// its timescale known.
//
// It is safe for concurrent use: the gate prepares inits on goroutines of
// their own while Intake reads timescales.
type initPreparer struct {
	cfg config.Config
	ci  *index.ChannelIndex
	// download is nil until the processor runs; an initPreparer without one
	// only reuses inits already on disk.
	download initDownloader
	prog     *progress.Counters

	mu sync.Mutex
	// timescales holds only validated, persisted init timescales, keyed by
	// init path (which names the period and representation).
	timescales map[string]uint32
}

func newInitPreparer(cfg config.Config, ci *index.ChannelIndex) *initPreparer {
	return &initPreparer{cfg: cfg, ci: ci, timescales: make(map[string]uint32)}
}

// timescale returns the track timescale of the usable init at path, reading
// an init already on disk the first time, or an error if there is none.
func (pr *initPreparer) timescale(path string) (uint32, error) {
	pr.mu.Lock()
	ts, ok := pr.timescales[path]
	pr.mu.Unlock()
	if ok {
		return ts, nil
	}
	ts, err := validate.ReadInitTimescale(path)
	if err != nil {
		return 0, err
	}
	pr.record(path, ts)
	return ts, nil
}

func (pr *initPreparer) record(path string, ts uint32) {
	pr.mu.Lock()
	pr.timescales[path] = ts
	pr.mu.Unlock()
}

// prepare makes the init of task's representation usable at path and returns
// its timescale. It first reuses a validated local init. Only init is
// retried; media already in memory keeps its retry budget. The deadline
// belongs to live work, while finite work is bounded by a retry budget even
// for a zero-value config.
func (pr *initPreparer) prepare(ctx context.Context, task *queue.SegmentTask, path string) (uint32, error) {
	ts, err := pr.prepareOnce(ctx, task, path)
	if err == nil {
		pr.record(path, ts)
	}
	return ts, err
}

func (pr *initPreparer) prepareOnce(ctx context.Context, task *queue.SegmentTask, path string) (uint32, error) {
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
	if rep := pr.ci.FindRep(task.PeriodID, task.ASID, task.RepID); rep != nil {
		for _, seg := range rep.Published() {
			if seg.InitPath == "" || seg.InitPath == path {
				continue
			}
			f, err := os.Open(seg.InitPath)
			if err != nil {
				break
			}
			limit := pr.cfg.Worker.MaxSegmentBytes
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
	if task.InitURL == "" || pr.download == nil {
		return 0, errors.New("fMP4 representation has no usable local init or init URL")
	}
	retries := pr.cfg.Worker.MaxRetriesStatic
	if retries <= 0 {
		retries = 5 // Same default as gateway configuration; never retry forever.
	}
	for attempt := 0; ; attempt++ {
		res := pr.download.FetchInit(ctx, task)
		err := res.Err
		backoff := 2 * time.Second
		if err == nil {
			if err = ctx.Err(); err == nil {
				var ts uint32
				ts, err = persistInit(path, res.Data)
				if err == nil {
					return ts, nil
				}
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		pr.prog.RecordSegmentError(StageInit, err.Error(), task.RepID, task.SegNo)
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
