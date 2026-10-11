package pipeline

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The gate's renewal, wired to the real preparer and fetcher: an init request
// that the first segment's live deadline cancels mid-flight is prepared again
// for a later segment already waiting, which is then stored without being
// fetched a second time. TestInitGate covers the renewal rules themselves.
func TestLaterDeadlinePendingMediaSurvives(t *testing.T) {
	var initCount atomic.Int32
	var mediaCount atomic.Int32
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			if initCount.Add(1) == 1 {
				close(started)
				<-r.Context().Done()
				return
			}
			_, _ = w.Write(fmp4Init(90000))
			return
		}
		mediaCount.Add(1)
		_, _ = w.Write(fmp4Segment(180000))
	}))
	t.Cleanup(origin.Close)
	first := initTask(1, origin.URL)
	first.Deadline = time.Now().Add(500 * time.Millisecond)
	pl := startInitPipeline(t, t.TempDir(), first)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("init did not start")
	}
	next := initTask(2, origin.URL)
	next.Deadline = time.Now().Add(time.Minute)
	pl.broker.Push(next)
	awaitInitCondition(t, "later media not fetched", func() bool { return mediaCount.Load() == 2 })
	awaitInitCondition(t, "tasks did not drain", pl.broker.Idle)
	pl.stop()
	if got := initCount.Load(); got != 2 {
		t.Fatalf("init requests = %d, want 2", got)
	}
	if got := mediaCount.Load(); got != 2 {
		t.Fatalf("media refetched: %d requests", got)
	}
	_ = onlySegment(t, pl.ci, next)
}
