package hlsingest

import (
	"context"
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

// A segment still downloading when the playlist is polled again must not be
// queued a second time once the segment before it has been stored: it is not
// a hole, the broker holds it as an attempt in flight. And a poll that finds
// no new segment reports none, so the playlist is reloaded sooner, whatever
// was queued again behind the live edge.
func TestASegmentInFlightIsNotQueuedAgain(t *testing.T) {
	w := newTestWatcher()
	v, st := testVariant(), &variantState{}
	if added := w.enqueueNew(context.Background(), v, st, mediaPlaylist(100, 3, 6.0)); added != 3 {
		t.Fatalf("first poll added %d, want 3", added)
	}

	stored := w.broker.Pop() // 100: stored
	slow := w.broker.Pop()   // 101: still downloading
	last := w.broker.Pop()   // 102: stored
	rep := w.ci.Period("0").AS(v.asID).Rep(v.RepID, 0)
	for _, a := range []uint64{stored.SegNo, last.SegNo} {
		rep.RestorePublished(index.SegmentState{SegNo: a, StartPTS: int64(a) * 540000, EndPTS: int64(a+1) * 540000, Timescale: TSTimescale})
	}
	stored.Finish()
	last.Finish()
	if slow.SegNo != 101 {
		t.Fatalf("in flight = %d, want 101", slow.SegNo)
	}

	added := w.enqueueNew(context.Background(), v, st, mediaPlaylist(100, 3, 6.0))
	if got := drain(w.broker); len(got) != 0 {
		t.Errorf("queued again while segment 101 is in flight: %v", keys(got))
	}
	if added != 0 {
		t.Errorf("an unchanged playlist reported %d new segments, want 0", added)
	}
	slow.Finish()
}

func keys[V any](m map[uint64]V) []uint64 {
	var out []uint64
	for k := range m {
		out = append(out, k)
	}
	return out
}
