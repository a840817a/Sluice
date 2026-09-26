package channel

import (
	"context"
	"sync"
	"testing"
	"time"
)

func waitForOperationUsers(t *testing.T, m *Manager, id string, want int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		m.operationLocksMu.Lock()
		got := 0
		if op := m.operationLocks[id]; op != nil {
			got = op.users
		}
		m.operationLocksMu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("operation lock users for %q = %d, want at least %d", id, got, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// A lifecycle operation on one channel must not hold up an unrelated config
// update. In particular, a Stop can be waiting for a counted VOD callback.
func TestBlockedStopDoesNotBlockOtherChannelUpdate(t *testing.T) {
	base := deadChannel()
	base.Title = "before"
	m, store, rtA := managerForConfigUpdate(t, "a", base)
	if err := store.Put("b", base); err != nil {
		t.Fatal(err)
	}
	rtB, err := m.startForTest(context.Background(), "b", base)
	if err != nil {
		t.Fatal(err)
	}

	stopping := make(chan struct{})
	cancel := rtA.cancel
	var cancelOnce sync.Once
	rtA.cancel = func() {
		cancel()
		cancelOnce.Do(func() { close(stopping) })
	}
	rtA.wg.Add(1)
	var releaseCallback sync.Once
	release := func() { releaseCallback.Do(rtA.wg.Done) }
	defer release()
	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop("a") }()
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel channel a")
	}

	started := make(chan error, 1)
	go func() {
		_, err := m.startForTest(context.Background(), "c", base)
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start on c: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start on c waited for Stop on a")
	}

	next := base
	next.Title = "after"
	result := make(chan error, 1)
	go func() {
		_, err := m.UpdateConfig(context.Background(), "b", next)
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateConfig on b waited for Stop on a")
	}
	if got := rtB.Config().Title; got != next.Title {
		t.Fatalf("runtime b title = %q, want %q", got, next.Title)
	}

	release()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not finish after its callback completed")
	}
}

// A Start for the channel being stopped waits until all work on the old runtime
// has drained, so it cannot write segment state alongside its predecessor.
func TestStartWaitsForSameChannelStop(t *testing.T) {
	m := newTestManager(t, configForTest(t))
	rt, err := m.startForTest(context.Background(), "ch", deadChannel())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)

	stopping := make(chan struct{})
	cancel := rt.cancel
	rt.cancel = func() {
		cancel()
		close(stopping)
	}
	rt.wg.Add(1)
	var releaseCallback sync.Once
	release := func() { releaseCallback.Do(rt.wg.Done) }
	defer release()
	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop("ch") }()
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel channel")
	}

	startDone := make(chan struct{})
	type startResult struct {
		rt  *Runtime
		err error
	}
	started := make(chan startResult, 1)
	go func() {
		replacement, startErr := m.startForTest(context.Background(), "ch", deadChannel())
		started <- startResult{rt: replacement, err: startErr}
		close(startDone)
	}()
	waitForOperationUsers(t, m, "ch", 2)
	select {
	case <-startDone:
		t.Fatal("Start returned while the prior runtime was still stopping")
	default:
	}

	release()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not finish")
	}
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement Start did not finish")
	}
	result := <-started
	if result.err != nil {
		t.Fatalf("replacement Start: %v", result.err)
	}
	replacement := result.rt
	if replacement == nil || replacement == rt || m.Get("ch") != replacement {
		t.Fatal("Start did not publish a replacement runtime after Stop")
	}
}

// StopAll takes the operation gate exclusively. A start that has already
// entered it must publish before StopAll detaches the runtimes, so shutdown
// cannot leave a newly started channel behind.
func TestStopAllDoesNotMissStartAlreadyInProgress(t *testing.T) {
	m := newTestManager(t, configForTest(t))
	unlock := m.lockOperation("ch")
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()

	startDone := make(chan struct{})
	go func() {
		_, _ = m.startForTest(context.Background(), "ch", deadChannel())
		close(startDone)
	}()
	waitForOperationUsers(t, m, "ch", 2)

	stopAllDone := make(chan struct{})
	go func() { m.StopAll(); close(stopAllDone) }()
	unlock()
	released = true

	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not finish")
	}
	select {
	case <-stopAllDone:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAll did not finish")
	}
	if rt := m.Get("ch"); rt != nil {
		t.Fatal("StopAll missed a start that was already in progress")
	}
	m.operationLocksMu.Lock()
	locks := len(m.operationLocks)
	m.operationLocksMu.Unlock()
	if locks != 0 {
		t.Fatalf("operation lock registry has %d entries after shutdown", locks)
	}
}
