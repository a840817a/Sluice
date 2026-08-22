// Package fetch provides the worker pool that downloads segments from upstream.
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

// httpError carries the upstream HTTP status code so callers can apply
// status-specific retry strategies (e.g. 410 Gone → drop, 425 Too Early → short backoff).
type httpError struct {
	statusCode int
	url        string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.statusCode, e.url)
}

// Result is the outcome of a single segment fetch attempt.
type Result struct {
	Task       *queue.SegmentTask
	Data       []byte // nil on error
	InitData   []byte // non-nil only on first fetch of this rep's init segment
	Err        error
	StatusCode int // upstream HTTP status when Err != nil; 0 for non-HTTP errors
}

// Fetcher wraps an http.Client and upstream credential configuration.
type Fetcher struct {
	client       *http.Client
	extraHeaders Headers // injected into every upstream request; may be rotated live
	// maxSegmentBytes bounds a full-file download. Range requests are bounded by
	// the range itself, which is tighter.
	maxSegmentBytes int64
}

// NewFetcher creates a Fetcher with the given HTTP client and upstream headers.
// Pass nil for headers if no upstream auth is required, and zero for
// maxSegmentBytes or timeout to accept the defaults.
//
// timeout builds the client's own http.Client.Timeout and therefore applies
// only when client is nil. Pass a client with its Timeout already set to
// override it; every production caller passes nil.
func NewFetcher(client *http.Client, headers Headers, maxSegmentBytes int64, timeout time.Duration) *Fetcher {
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
		extraHeaders:    headers,
		maxSegmentBytes: maxSegmentBytes,
	}
}

// Fetch downloads the segment described by task.
// It always fetches the media segment; it also fetches the init segment when
// needInit is true and task.InitURL is set.
// Byte-range requests are used when task.RangeEnd > 0 (media) or
// task.InitRangeEnd > 0 (init).
func (f *Fetcher) Fetch(ctx context.Context, task *queue.SegmentTask, needInit bool) Result {
	r := Result{Task: task}

	// Fetch media segment
	var err error
	if task.RangeEnd > 0 {
		r.Data, err = f.getRange(ctx, task.URL, task.RangeStart, task.RangeEnd)
	} else {
		r.Data, err = f.get(ctx, task.URL)
	}
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			r.StatusCode = he.statusCode
		}
		r.Err = fmt.Errorf("segment %s: %w", task.URL, err)
		return r
	}

	// Fetch init segment if needed
	if needInit && task.InitURL != "" {
		var initData []byte
		if task.InitRangeEnd > 0 {
			initData, err = f.getRange(ctx, task.InitURL, task.InitRangeStart, task.InitRangeEnd)
		} else {
			initData, err = f.get(ctx, task.InitURL)
		}
		if err != nil {
			var he *httpError
			if errors.As(err, &he) {
				r.StatusCode = he.statusCode
			}
			r.Err = fmt.Errorf("init %s: %w", task.InitURL, err)
			return r
		}
		r.InitData = initData
	}

	return r
}

func (f *Fetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	ApplyHeaders(req, f.extraHeaders)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{statusCode: resp.StatusCode, url: url}
	}

	// When the origin declares a size, refuse an oversized body before reading a
	// single byte of it.
	if resp.ContentLength > f.maxSegmentBytes {
		slog.Warn("upstream segment exceeds the size limit; refusing",
			"url", url, "content_length", resp.ContentLength, "limit", f.maxSegmentBytes)
		return nil, ErrTooLarge
	}
	data, err := ReadAtMost(resp.Body, f.maxSegmentBytes)
	if errors.Is(err, ErrTooLarge) {
		slog.Warn("upstream segment exceeds the size limit; refusing",
			"url", url, "limit", f.maxSegmentBytes)
	}
	return data, err
}

func (f *Fetcher) getRange(ctx context.Context, url string, start, end uint64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("invalid byte range %d-%d for %s", start, end, url)
	}
	// The request states exactly how many bytes it wants, so that count is the
	// bound — tighter than the segment ceiling, and it also catches an origin
	// that ignores Range and replies with the whole file. The ceiling still
	// applies in case the manifest asked for an absurd range in the first place.
	limit := int64(end-start) + 1
	if limit <= 0 || limit > f.maxSegmentBytes {
		limit = f.maxSegmentBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	ApplyHeaders(req, f.extraHeaders)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Servers may return 206 Partial Content or 200 OK (full content).
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, &httpError{statusCode: resp.StatusCode, url: fmt.Sprintf("%s (range %d-%d)", url, start, end)}
	}

	data, err := ReadAtMost(resp.Body, limit)
	if errors.Is(err, ErrTooLarge) {
		slog.Warn("upstream returned more than the requested byte range; refusing",
			"url", url, "range_start", start, "range_end", end, "limit", limit)
	}
	return data, err
}

// Pool manages N concurrent fetch workers draining a Broker queue.
type Pool struct {
	fetcher    *Fetcher
	broker     *queue.Broker
	numWorkers int
	results    chan Result
	// initSeen tracks which rep init segments have been fetched: repKey → true
	initSeen map[string]bool
	initMu   chan struct{} // 1-buffered mutex
}

// NewPool creates a worker pool but does not start it yet.
func NewPool(fetcher *Fetcher, broker *queue.Broker, workers int) *Pool {
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	return &Pool{
		fetcher:    fetcher,
		broker:     broker,
		numWorkers: workers,
		results:    make(chan Result, workers*4),
		initSeen:   make(map[string]bool),
		initMu:     mu,
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

		task := p.broker.Pop()
		if task == nil {
			// Queue empty or next task not ready; yield briefly
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}

		needInit := p.markInitIfNeeded(task)
		result := p.fetcher.Fetch(ctx, task, needInit)
		if needInit && result.Err != nil {
			p.unmarkInit(task)
		}

		select {
		case p.results <- result:
		case <-ctx.Done():
			return
		}
	}
}

// markInitIfNeeded returns true if the init segment for this rep hasn't been
// fetched yet, and marks it as in-progress atomically.
func (p *Pool) markInitIfNeeded(task *queue.SegmentTask) bool {
	key := initKey(task)
	<-p.initMu
	defer func() { p.initMu <- struct{}{} }()
	if p.initSeen[key] {
		return false
	}
	p.initSeen[key] = true
	return true
}

func (p *Pool) unmarkInit(task *queue.SegmentTask) {
	key := initKey(task)
	<-p.initMu
	defer func() { p.initMu <- struct{}{} }()
	delete(p.initSeen, key)
}

func initKey(task *queue.SegmentTask) string {
	return task.ChannelID + "/" + task.PeriodID + "/" + task.ASID + "/" + task.RepID
}
