package channel

import (
	"sync"
	"testing"
	"time"
)

// launch closes each exit signal only once everything it stands for has
// returned — sourceDone the source, pipelineDone both the pool and the
// processor — and counts every goroutine it starts, so a Stop that waits on
// the runtime's WaitGroup outlives them all.
func TestChannelIngestLaunchSignalsExits(t *testing.T) {
	c := newChannelIngest(func() {}, func() {}, nil, nil)
	pool, processor, source := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	c.launch(&wg, func() { <-pool }, func() { <-processor }, func() { <-source })

	open := func(name string, ch chan struct{}) {
		t.Helper()
		select {
		case <-ch:
			t.Fatalf("%s closed early", name)
		case <-time.After(20 * time.Millisecond):
		}
	}
	closed := func(name string, ch chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s never closed", name)
		}
	}
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()

	open("sourceDone", c.sourceDone)
	close(source)
	closed("sourceDone", c.sourceDone)

	close(pool)
	open("pipelineDone with the processor still running", c.pipelineDone)
	open("the WaitGroup with the processor still running", joined)
	close(processor)
	closed("pipelineDone", c.pipelineDone)
	closed("the WaitGroup", joined)
}
