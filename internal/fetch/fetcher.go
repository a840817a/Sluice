// Package fetch provides the worker pool that downloads segments from origins.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/queue"
)

// Result is the outcome of a single segment fetch attempt.
type Result struct {
	// Attempt owns completion for pool results; direct Fetch calls have none.
	Attempt *queue.Attempt
	Task    *queue.SegmentTask
	// Data is the fetched body, nil on error: the media segment for Fetch,
	// the init segment for FetchInit.
	Data       []byte
	Err        error
	StatusCode int // origin HTTP status when Err != nil; 0 for non-HTTP errors
}

// Fetcher downloads segments and inits through the channel's Origin.
type Fetcher struct {
	client *http.Client
	origin *Origin
	// maxSegmentBytes bounds a full-file download. Range requests are bounded by
	// the range itself, which is tighter.
	maxSegmentBytes int64
}

// NewFetcher creates a Fetcher with the given HTTP client and Origin. Pass nil
// for origin if the origin needs no headers, and zero for maxSegmentBytes or
// timeout to accept the defaults.
//
// timeout builds the client's own http.Client.Timeout and therefore applies
// only when client is nil. Pass a client with its Timeout already set to
// override it; every production caller passes nil.
func NewFetcher(client *http.Client, origin *Origin, maxSegmentBytes int64, timeout time.Duration) *Fetcher {
	if timeout <= 0 {
		timeout = DefaultSegmentTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	if maxSegmentBytes <= 0 {
		maxSegmentBytes = DefaultMaxSegmentBytes
	}
	return &Fetcher{
		client:          client,
		origin:          origin,
		maxSegmentBytes: maxSegmentBytes,
	}
}

// Fetch downloads the media segment described by task, as a byte range when
// task.RangeEnd > 0.
func (f *Fetcher) Fetch(ctx context.Context, task *queue.SegmentTask) Result {
	r := Result{Task: task}
	var err error
	if task.RangeEnd > 0 {
		r.Data, err = f.getRange(ctx, task.URL, task.RangeStart, task.RangeEnd)
	} else {
		r.Data, err = f.get(ctx, task.URL)
	}
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			r.StatusCode = se.Code
		}
		r.Err = fmt.Errorf("segment %s: %w", task.URL, err)
	}
	return r
}

// FetchInit downloads the init segment of task's representation, as a byte
// range when task.InitRangeEnd > 0. It only downloads bytes; validation and
// readiness belong to ingest, which fetches inits apart from the media
// workers and their bounded result channel, so media backpressure cannot
// prevent its prerequisite from completing.
func (f *Fetcher) FetchInit(ctx context.Context, task *queue.SegmentTask) Result {
	r := Result{Task: task}
	if task.InitURL == "" {
		r.Err = errors.New("fMP4 representation has no init URL")
		return r
	}
	var err error
	if task.InitRangeEnd > 0 {
		r.Data, err = f.getRange(ctx, task.InitURL, task.InitRangeStart, task.InitRangeEnd)
	} else {
		r.Data, err = f.get(ctx, task.InitURL)
	}
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			r.StatusCode = se.Code
		}
		r.Err = fmt.Errorf("init %s: %w", task.InitURL, err)
	}
	return r
}

func (f *Fetcher) get(ctx context.Context, url string) ([]byte, error) {
	data, err := f.origin.Get(ctx, f.client, url, f.maxSegmentBytes)
	if errors.Is(err, ErrTooLarge) {
		slog.Warn("upstream segment exceeds the size limit; refusing",
			"url", url, "limit", f.maxSegmentBytes)
	}
	return data, err
}

// getRange's read is bounded by the range itself, which is tighter than the
// segment ceiling; the ceiling still applies in case the manifest asked for
// an absurd range in the first place.
func (f *Fetcher) getRange(ctx context.Context, url string, start, end uint64) ([]byte, error) {
	data, err := f.origin.GetRange(ctx, f.client, url, start, end, f.maxSegmentBytes)
	if errors.Is(err, ErrTooLarge) {
		slog.Warn("upstream returned more than the requested byte range; refusing",
			"url", url, "range_start", start, "range_end", end)
	}
	return data, err
}

// Pool manages N concurrent fetch workers draining a Broker queue.
type Pool struct {
	fetcher    *Fetcher
	broker     *queue.Broker
	numWorkers int
	results    chan Result
}

// NewPool creates a worker pool but does not start it yet.
func NewPool(fetcher *Fetcher, broker *queue.Broker, workers int) *Pool {
	workers = max(1, workers)
	return &Pool{
		fetcher:    fetcher,
		broker:     broker,
		numWorkers: workers,
		results:    make(chan Result, workers*4),
	}
}

// Results returns the channel on which fetch outcomes are published.
func (p *Pool) Results() <-chan Result {
	return p.results
}

// Run starts the worker goroutines and blocks until ctx is cancelled and every
// worker has returned.
//
// Joining the workers is what makes stopping a channel safe: the caller that
// cancels ctx needs to know that nothing is still fetching or, worse, still
// writing, before it hands the channel's data directory to a replacement. Once
// the workers are gone there are no senders left, so results is closed to give
// the processor a clean end-of-stream instead of a bare cancellation.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	close(p.results)
}

func (p *Pool) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		attempt := p.broker.Pop()
		if attempt == nil {
			// Queue empty or next task not ready; yield briefly
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}

		result := p.fetcher.Fetch(ctx, attempt.SegmentTask)
		result.Attempt = attempt

		select {
		case p.results <- result:
		case <-ctx.Done():
			attempt.Finish()
			return
		}
	}
}

// Fetcher is the Fetcher the pool's workers use, for requests ingest makes
// apart from them, such as inits.
func (p *Pool) Fetcher() *Fetcher { return p.fetcher }
