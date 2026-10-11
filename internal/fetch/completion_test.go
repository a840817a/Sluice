package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/queue"
)

func TestPoolCancellationSettlesUnsentAttempt(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte("media")) }))
	defer origin.Close()
	broker := queue.NewBroker(10)
	for n := uint64(1); n <= 6; n++ {
		broker.Push(&queue.SegmentTask{SegNo: n, URL: origin.URL, Format: queue.FormatTS})
	}
	pool := NewPool(NewFetcher(origin.Client(), nil, 0, time.Second), broker, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 5 {
		t.Fatal("worker did not reach full result channel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pool did not stop")
	}
	count := 0
	for res := range pool.Results() {
		count++
		if !res.Attempt.Finish() || res.Attempt.Finish() {
			t.Fatal("result completion was lost or duplicated")
		}
	}
	if count != 4 {
		t.Fatalf("buffered results=%d, want 4", count)
	}
	for a := broker.Pop(); a != nil; a = broker.Pop() {
		a.Finish()
	}
	if !broker.Idle() {
		t.Fatal("unsent result leaked its attempt")
	}
}
