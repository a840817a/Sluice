package pipeline

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLaterLiveMediaRecoversInit(t *testing.T) {
	var ready atomic.Bool
	var initCount atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			initCount.Add(1)
			if !ready.Load() {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write(fmp4Init(90000))
			return
		}
		_, _ = w.Write(fmp4Segment(180000))
	}))
	t.Cleanup(origin.Close)
	first := initTask(1, origin.URL)
	first.Deadline = time.Now().Add(time.Minute)
	pl := startInitPipeline(t, t.TempDir(), first)
	awaitInitCondition(t, "first failure did not drain", pl.broker.Idle)
	ready.Store(true)
	next := initTask(2, origin.URL)
	next.Deadline = time.Now().Add(time.Minute)
	pl.broker.Push(next)
	awaitInitCondition(t, "later task did not drain", pl.broker.Idle)
	pl.stop()
	if got := initCount.Load(); got != 3 {
		t.Fatalf("init requests = %d, want 3", got)
	}
	_ = onlySegment(t, pl.ci, next)
}
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
