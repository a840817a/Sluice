package dashingest

import (
	"context"
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

// A segment still downloading when the next MPD arrives must not be queued a
// second time, even once a later segment has been stored around it: the
// broker already holds that work, as an attempt in flight.
func TestASegmentInFlightIsNotQueuedAgain(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishImmediately)
	w, broker := newRestartWatcher(ci)
	w.processMPD(context.Background(), liveMPD(t, ast0, 1, 4))

	var slow = broker.Pop()
	for a := broker.Pop(); a != nil; a = broker.Pop() {
		ci.Period(a.PeriodID).AS(a.ASID).Rep(a.RepID, 0).RestorePublished(index.SegmentState{SegNo: a.SegNo})
		a.Finish()
	}
	if slow == nil || slow.SegNo != 1 {
		t.Fatalf("first popped = %+v, want segment 1", slow)
	}

	w.processMPD(context.Background(), liveMPD(t, ast0, 1, 4))
	if got := drain(broker); len(got) != 0 {
		t.Errorf("queued again while segment 1 is in flight: %v", got)
	}
	slow.Finish()
}
