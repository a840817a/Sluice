package pipeline

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// A failed fetch reaches the sole result consumer while discovery has already
// filled the slot it left behind. Retrying must neither shed finite work nor
// wait for that consumer to make room. Terminal failures retain their rules.
func TestFiniteRetryThroughProcessorWithFullBroker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		retries   int
		wantRetry bool
		wantDrops uint64
	}{
		{name: "too early", status: http.StatusTooEarly, wantRetry: true},
		{name: "temporary failure", status: http.StatusServiceUnavailable, wantRetry: true},
		{name: "permanently gone", status: http.StatusGone},
		{name: "retry budget exhausted", status: http.StatusServiceUnavailable, retries: 1, wantDrops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := queue.NewBroker(1)
			failed := &queue.SegmentTask{ChannelID: "retry", RepID: "v", SegNo: 1, Retries: tc.retries}
			if !broker.PushPreserving(context.Background(), failed) {
				t.Fatal("failed to admit first task")
			}
			attempt := broker.Pop()
			if attempt == nil || attempt.SegmentTask != failed {
				t.Fatal("first task was not fetched")
			}
			next := &queue.SegmentTask{ChannelID: "retry", RepID: "v", SegNo: 2}
			if !broker.PushPreserving(context.Background(), next) {
				t.Fatal("failed to fill pending queue")
			}
			processor := NewProcessor(config.Config{
				Worker: config.WorkerConfig{MaxRetriesStatic: 1},
			}, "retry", index.NewChannelIndex(index.PublishAVBarrier), broker)
			done := make(chan struct{})
			retryStarted := time.Now()
			go func() {
				processor.handle(fetch.Result{Task: failed, Attempt: attempt, StatusCode: tc.status, Err: errors.New("origin failure")})
				attempt.Finish() // Idempotent after Retry.
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				// Free the queue so an incorrectly blocking retry can exit.
				if pending := broker.Pop(); pending != nil {
					pending.Finish()
				}
				select {
				case <-done:
				case <-time.After(time.Second):
				}
				t.Fatal("result consumer blocked retrying into a full broker")
			}
			if got := broker.Dropped(); got != tc.wantDrops {
				t.Fatalf("Dropped = %d, want %d", got, tc.wantDrops)
			}
			if broker.Idle() {
				t.Fatal("broker reports completion before queued work is stored")
			}
			got := broker.Pop()
			if got == nil || got.SegmentTask != next {
				t.Fatalf("next task was displaced: got %v", got)
			}
			got.Finish()
			if tc.wantRetry {
				if broker.Idle() {
					t.Fatal("broker reports completion while retry is pending")
				}
				if failed.Retries != 1 || !failed.NextRetry.After(retryStarted) {
					t.Fatal("retry did not preserve its attempt count and backoff")
				}
				// Handling has finished; skip the real backoff before draining.
				failed.NextRetry = time.Time{}
				got := broker.Pop()
				if got == nil || got.SegmentTask != failed {
					t.Fatalf("failed segment was lost instead of retried: got %v", got)
				}
				got.Finish()
			}
			if !broker.Idle() {
				t.Fatal("broker did not become idle after all terminal results")
			}
		})
	}
}
