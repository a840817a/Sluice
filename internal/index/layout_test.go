package index

import (
	"reflect"
	"sync"
	"testing"
)

// Layout reports every level of the index, ordered by ID, including a period
// with no AdaptationSet and an AdaptationSet with no representation, which
// ForEachRep never yields.
func TestLayoutReportsEveryLevelInOrder(t *testing.T) {
	ci := NewChannelIndex()
	ci.Period("empty")
	ci.Period("p").AS("1").SetMediaType(MediaAudio) // no representation yet
	v := ci.Period("p").AS("0")
	v.SetMediaType(MediaVideo)
	v.Rep("v1", 8).RestorePublished(SegmentState{SegNo: 1, EndPTS: 1, Timescale: 1})
	v0 := v.Rep("v0", 8)
	v0.RestorePublished(SegmentState{SegNo: 1, EndPTS: 1, Timescale: 1})
	v0.RestorePublished(SegmentState{SegNo: 2, StartPTS: 1, EndPTS: 2, Timescale: 1})
	v0.commit(SegmentState{SegNo: 3, StartPTS: 2, EndPTS: 3, Timescale: 1})
	v0.ExpireOlderThan(1.5)

	want := []PeriodLayout{
		{ID: "empty", AdaptationSets: []AdaptationSetLayout{}},
		{ID: "p", AdaptationSets: []AdaptationSetLayout{
			{ID: "0", MediaType: MediaVideo, Reps: []RepLayout{
				{ID: "v0", Published: 1, Committed: 1, Expired: 1},
				{ID: "v1", Published: 1},
			}},
			{ID: "1", MediaType: MediaAudio, Reps: []RepLayout{}},
		}},
	}
	if got := ci.Layout(); !reflect.DeepEqual(got, want) {
		t.Errorf("Layout =\n%+v\nwant\n%+v", got, want)
	}
}

// The index's readers take its locks themselves: run under -race, a PeriodIDs
// or Layout that read the period map unlocked while a commit added a period
// would be reported.
func TestIndexReadsLockThemselves(t *testing.T) {
	ci := NewChannelIndex()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ci.Commit(RepRef{PeriodID: string(rune('a'+i%26)) + "x", ASID: "0", RepID: "v0", MediaType: MediaVideo}, 4,
				SegmentState{SegNo: uint64(i), EndPTS: 1, Timescale: 1})
		}
	}()
	for i := 0; i < 200; i++ {
		_ = ci.PeriodIDs()
		_ = ci.Layout()
	}
	wg.Wait()
}
