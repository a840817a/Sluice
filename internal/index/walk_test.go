package index

import (
	"sort"
	"testing"
	"time"
)

func seedIndex(t *testing.T) *ChannelIndex {
	t.Helper()
	ci := NewChannelIndex()
	for _, p := range []struct {
		period, as, mediaType string
		reps                  []string
	}{
		{"p0", "0", "video", []string{"v0", "v1"}},
		{"p0", "1", "audio", []string{"a0"}},
		{"p1", "0", "video", []string{"v0"}},
	} {
		as := ci.Period(p.period).AS(p.as)
		as.SetMediaType(p.mediaType)
		for _, r := range p.reps {
			as.Rep(r, 8)
		}
	}
	return ci
}

func TestForEachRepVisitsEveryRepresentation(t *testing.T) {
	ci := seedIndex(t)

	var got []string
	ci.ForEachRep(func(ref RepRef, rep *RepresentationState) {
		got = append(got, ref.PeriodID+"/"+ref.ASID+"/"+ref.RepID+"/"+ref.MediaType)
		if rep == nil {
			t.Error("nil representation passed to fn")
		}
	})
	sort.Strings(got)

	want := []string{
		"p0/0/v0/video",
		"p0/0/v1/video",
		"p0/1/a0/audio",
		"p1/0/v0/video",
	}
	if len(got) != len(want) {
		t.Fatalf("visited %d reps, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("visit[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// fn runs while the AdaptationSet lock is held, so representation methods —
// which take their own lock — must remain callable from inside it. If this ever
// deadlocks the test times out rather than failing, which is the signal.
func TestForEachRepAllowsRepMethodsInsideTheCallback(t *testing.T) {
	ci := seedIndex(t)
	ci.Period("p0").AS("0").Rep("v0", 8).Commit(SegmentState{SegNo: 1, Timescale: 90000})

	var total int
	ci.ForEachRep(func(_ RepRef, rep *RepresentationState) {
		_, committed, _ := rep.StatusCounts()
		total += committed
		_ = rep.Published()
		_ = rep.Total()
	})
	if total != 1 {
		t.Errorf("committed across the index = %d, want 1", total)
	}
}

func TestForEachRepOnEmptyIndex(t *testing.T) {
	calls := 0
	NewChannelIndex().ForEachRep(func(RepRef, *RepresentationState) { calls++ })
	if calls != 0 {
		t.Errorf("fn called %d times on an empty index, want 0", calls)
	}
}

func TestForEachRepUntilStopsEarly(t *testing.T) {
	ci := seedIndex(t)

	calls := 0
	ci.ForEachRepUntil(func(RepRef, *RepresentationState) bool {
		calls++
		return false // stop on the very first representation
	})
	if calls != 1 {
		t.Errorf("fn called %d times after returning false, want 1", calls)
	}

	// Returning true throughout must visit everything, like ForEachRep.
	calls = 0
	ci.ForEachRepUntil(func(RepRef, *RepresentationState) bool {
		calls++
		return true
	})
	if calls != 4 {
		t.Errorf("full walk visited %d reps, want 4", calls)
	}
}

// An early exit must leave every lock released, or the next writer blocks
// forever. A leaked read lock makes this test hang rather than fail, which is
// the signal.
func TestForEachRepUntilReleasesLocksOnEarlyExit(t *testing.T) {
	ci := seedIndex(t)

	ci.ForEachRepUntil(func(RepRef, *RepresentationState) bool { return false })

	done := make(chan struct{})
	go func() {
		ci.Period("p2").AS("0").Rep("v9", 8) // needs the write lock at every level
		ci.ForEachRep(func(RepRef, *RepresentationState) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("index still locked 5s after an early-exit walk")
	}
}

func TestPeriodStateForEachRep(t *testing.T) {
	ci := seedIndex(t)

	var got []string
	ci.Period("p0").ForEachRep(func(asID, mediaType string, rep *RepresentationState) {
		got = append(got, asID+"/"+rep.ID+"/"+mediaType)
	})
	sort.Strings(got)

	want := []string{"0/v0/video", "0/v1/video", "1/a0/audio"}
	if len(got) != len(want) {
		t.Fatalf("visited %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("visit[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Representation methods must stay callable from inside the callback.
	ci.Period("p0").AS("0").Rep("v0", 8).Commit(SegmentState{SegNo: 1, Timescale: 90000})
	committed := 0
	ci.Period("p0").ForEachRep(func(_, _ string, rep *RepresentationState) {
		_, c, _ := rep.StatusCounts()
		committed += c
	})
	if committed != 1 {
		t.Errorf("committed in p0 = %d, want 1", committed)
	}
}

func TestFindRep(t *testing.T) {
	ci := seedIndex(t)

	if rep := ci.FindRep("p0", "1", "a0"); rep == nil {
		t.Error("FindRep(p0,1,a0) = nil, want the audio representation")
	} else if rep.ID != "a0" {
		t.Errorf("FindRep returned rep %q, want a0", rep.ID)
	}

	for _, miss := range [][3]string{
		{"nope", "0", "v0"},
		{"p0", "nope", "v0"},
		{"p0", "0", "nope"},
	} {
		if rep := ci.FindRep(miss[0], miss[1], miss[2]); rep != nil {
			t.Errorf("FindRep%v = %v, want nil", miss, rep.ID)
		}
	}

	// A lookup must not create anything, unlike Period/AS/Rep.
	ci.FindRep("ghost", "0", "v0")
	ci.mu.RLock()
	_, created := ci.Periods["ghost"]
	ci.mu.RUnlock()
	if created {
		t.Error("FindRep created a period; it must be read-only")
	}
}
