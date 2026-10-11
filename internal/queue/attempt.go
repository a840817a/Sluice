package queue

import "time"

// Attempt owns completion of one Pop. Carry it with the fetched result until
// storage, rejection or cancellation. Finish and Retry are mutually exclusive;
// repeating either, even through a copied handle, cannot affect another attempt.
// The task belongs to this attempt until completion. Do not mutate it afterward:
// a successful Retry may immediately hand it to another worker.
type Attempt struct {
	*SegmentTask
	completion *completion
}

type completion struct {
	broker   *Broker
	task     *SegmentTask
	finished bool // guarded by broker.mu, including task requeue and outstanding
	// promote is a finite source's copy of task, admitted while task was in
	// flight; settle upgrades task with it (promotePreserved). Guarded by
	// broker.mu.
	promote *SegmentTask
}

// Finish settles this attempt. It returns false if it was already settled.
func (a *Attempt) Finish() bool {
	return a.completion.settle(nil)
}

// Retry settles this attempt and atomically hands its work back to the queue.
// True means the work is queued. No copy of it can be queued already: while
// the attempt was in flight, Push and PushPreserving took the same work as
// admitted (Broker.inFlight), and a finite copy's policy is applied here.
// False means it was dropped or this attempt had already completed. Callers
// never need a separate Finish after Retry, including when capacity rejects it.
func (a *Attempt) Retry(maxRetries int, backoff time.Duration) bool {
	c := a.completion
	return c.settle(func() bool {
		return c.broker.requeueLocked(c.task, maxRetries, backoff)
	})
}

// settle owns the once-only transition. Any retry runs under the same lock as
// the count update, so observers cannot see an idle gap during the handover.
func (c *completion) settle(retry func() bool) bool {
	c.broker.mu.Lock()
	defer c.broker.mu.Unlock()
	if c.finished {
		return false
	}
	c.finished = true
	// The work leaves flight before any retry queues it again.
	if key := c.task.Key(); c.broker.inFlight[key] == c {
		delete(c.broker.inFlight, key)
	}
	if c.promote != nil {
		promotePreserved(c.task, c.promote)
	}
	queued := true
	if retry != nil {
		queued = retry()
	}
	c.broker.outstanding--
	return queued
}
