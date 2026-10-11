package queue

import (
	"sync"
	"testing"
)

func TestAttemptCompletionIsOncePerPop(t *testing.T) {
	b := NewBroker(2)
	b.Push(makeTask(1, 0))
	b.Push(makeTask(2, 1))
	first, second := b.Pop(), b.Pop()
	copyOfFirst := *first
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); copyOfFirst.Finish() }()
	}
	wg.Wait()
	if first.Finish() || b.Idle() {
		t.Fatal("duplicate completion consumed another attempt")
	}
	if !second.Finish() || !b.Idle() {
		t.Fatal("second attempt did not finish")
	}
}

func TestRetryCompletesOldAttemptWithoutCompletingNewOne(t *testing.T) {
	b := NewBroker(1)
	b.Push(makeTask(1, 0))
	old := b.Pop()
	if !old.Retry(2, 0) || b.Idle() {
		t.Fatal("retry lost work")
	}
	next := b.Pop()
	if next == nil {
		t.Fatal("retry missing")
	}
	if old.Finish() || old.Retry(2, 0) || b.Idle() {
		t.Fatal("old attempt changed current work")
	}
	if next.Retries != 1 || b.Len() != 0 {
		t.Fatal("duplicate retry changed budget or queue")
	}
	next.Finish()
	if !b.Idle() {
		t.Fatal("retry leaked outstanding work")
	}
}

func TestRetryReportsCapacityDropAndCompletesAttempt(t *testing.T) {
	b := NewBroker(1)
	b.Push(makeTask(1, 5))
	a := b.Pop()
	b.Push(makeTask(2, 0))
	if a.Retry(2, 0) {
		t.Fatal("capacity rejected retry reported queued")
	}
	if a.Finish() {
		t.Fatal("retry did not complete original attempt")
	}
	next := b.Pop()
	if next == nil || next.SegNo != 2 || b.Dropped() != 1 {
		t.Fatal("priority policy changed")
	}
	next.Finish()
	if !b.Idle() {
		t.Fatal("dropped retry leaked outstanding work")
	}
}

func TestRetryAndFinishRaceHasOneWinner(t *testing.T) {
	for range 100 {
		b := NewBroker(1)
		b.Push(makeTask(1, 0))
		a := b.Pop()
		retryResult, finishResult := make(chan bool, 1), make(chan bool, 1)
		go func() { retryResult <- a.Retry(1, 0) }()
		go func() { finishResult <- a.Finish() }()
		retried, finished := <-retryResult, <-finishResult
		if retried == finished {
			t.Fatalf("retry=%v finish=%v; want one winner", retried, finished)
		}
		if retried {
			next := b.Pop()
			if next == nil {
				t.Fatal("successful retry vanished")
			}
			next.Finish()
		}
		if !b.Idle() {
			t.Fatal("completion race leaked work")
		}
	}
}
