package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// Exclusive runs its function while the processor runs, and refuses once it
// has stopped, so a caller can tell a skipped function from one that ran.
func TestProcessorExclusive(t *testing.T) {
	broker := queue.NewBroker(4)
	pool := fetch.NewPool(fetch.NewFetcher(nil, fetch.NewOrigin(nil, "", ""), 0, 0), broker, 1)
	p := NewProcessor(config.Config{}, "c", index.NewChannelIndex(index.PublishAVBarrier), broker)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); p.Run(ctx, pool) }()

	want := errors.New("fn's error")
	ran := false
	if err := p.Exclusive(context.Background(), func() error { ran = true; return want }); err != want || !ran {
		t.Errorf("Exclusive while running: ran %v, err %v; want fn run and its error", ran, err)
	}

	cancel()
	<-stopped
	err := p.Exclusive(context.Background(), func() error {
		t.Error("fn ran after the processor stopped")
		return nil
	})
	if !errors.Is(err, ErrStopped) {
		t.Errorf("Exclusive after Run returned: %v, want ErrStopped", err)
	}
}
