package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	imdp "github.com/a840817a/sluice/internal/mpd"
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

// launchIngest also starts the watch for a finished origin: a live channel
// whose source says the origin is a finished asset is shown as static
// ingesting while the source is still enumerating it, not only once the
// source returns.
func TestLaunchIngestWatchesForAFinishedOrigin(t *testing.T) {
	m := newTestManager(t, configForTest(t))
	cfg := deadChannel()
	pl := newIngestPipeline(m.gwCfg, "c", cfg)
	src := &fakeSource{} // Run blocks until its context ends
	src.finished.Store(true)
	rt := &Runtime{ID: "c", Index: pl.index, Source: src, Progress: pl.progress, Broker: pl.broker}
	rt.setConfig(cfg)
	rt.modes.begin(ModeLive, false, 0)

	lifecycleCtx, cancel := context.WithCancel(context.Background())
	sourceCtx, sourceCancel := context.WithCancel(lifecycleCtx)
	poolCtx, poolCancel := context.WithCancel(lifecycleCtx)
	ingest := newChannelIngest(sourceCancel, poolCancel, pl.broker, func() *imdp.ParsedMPD { return nil })
	m.launchIngest(sourceCtx, poolCtx, lifecycleCtx, rt, cfg, pl, ingest)
	t.Cleanup(func() { cancel(); rt.wg.Wait() })

	deadline := time.Now().Add(5 * staticOriginPollInterval)
	for rt.Mode() != ModeStaticIngesting && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := rt.Mode(); got != ModeStaticIngesting {
		t.Errorf("mode = %s with the source still running, want %s", got, ModeStaticIngesting)
	}
}
