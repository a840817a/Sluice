package index

import (
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"testing"
)

func TestRepresentationPeriodsRetainPublishedHistory(t *testing.T) {
	ci := NewChannelIndex(PublishAVBarrier)
	rep := ci.Period("10").AS("0").Rep("v", 2)
	for _, n := range []uint64{100, 101, 103, 102} {
		rep.RestorePublished(SegmentState{SegNo: n, Discontinuity: n == 101, EndPTS: int64(n), Timescale: 1, InitPath: "init"})
	}
	rep.commit(SegmentState{SegNo: 104, Discontinuity: true, InitPath: "pending"})
	ci.Period("2").AS("0").Rep("v", 0).RestorePublished(SegmentState{SegNo: 1})
	ci.Period("1").AS("0").Rep("other", 0).RestorePublished(SegmentState{SegNo: 99})
	got := ci.RepresentationPeriods("v")
	if len(got) != 2 || got[0].PeriodID != "2" || got[1].PeriodID != "10" {
		t.Fatalf("periods = %+v", got)
	}
	p := got[1]
	if !slices.Equal(segNos(p.Segments), []uint64{102}) || !p.RangeKnown || p.First != 100 || p.Last != 103 || !slices.Equal(p.Discontinuities, []uint64{101}) || p.InitPath != "init" {
		t.Fatalf("published window/history/init = %+v", p)
	}
	rep.ExpireOlderThan(200)
	p = ci.RepresentationPeriods("v")[1]
	if len(p.Segments) != 0 || p.First != 100 || p.Last != 103 || !p.RangeKnown || !slices.Equal(p.Discontinuities, []uint64{101}) || p.InitPath != "pending" {
		t.Fatalf("expired window lost history/fallback: %+v", p)
	}
}

func TestRepresentationReadsAreDetached(t *testing.T) {
	ci := NewChannelIndex(PublishImmediately)
	rep := ci.Period("0").AS("0").Rep("v", 2)
	iv := []byte{1, 2}
	rep.RestorePublished(SegmentState{SegNo: 1, IV: iv, Discontinuity: true, InitPath: "old"})
	periods := ci.RepresentationPeriods("v")
	snap := ci.Snapshot()
	iv[0] = 9
	if periods[0].Segments[0].IV[0] != 1 || snap.Published("0", "0", "v")[0].IV[0] != 1 {
		t.Fatal("read IV aliases live state")
	}
	periods[0].Segments[0].IV[1] = 8
	periods[0].Discontinuities[0] = 99
	if fresh := ci.RepresentationPeriods("v")[0]; fresh.Segments[0].IV[1] != 2 || fresh.Discontinuities[0] != 1 {
		t.Fatal("reader changed live state")
	}
	// Replacement and eviction must not alter any field already returned.
	before := ci.RepresentationPeriods("v")[0]
	rep.RestorePublished(SegmentState{SegNo: 2, Discontinuity: true, InitPath: "new"})
	rep.RestorePublished(SegmentState{SegNo: 3, InitPath: "new"})
	rep.ExpireOlderThan(1)
	if before.Last != 1 || before.InitPath != "old" || !slices.Equal(segNos(before.Segments), []uint64{1}) || !slices.Equal(before.Discontinuities, []uint64{1}) {
		t.Fatalf("later writes changed read: %+v", before)
	}
}

func TestRepresentationReadIsCoherentDuringPublication(t *testing.T) {
	ci := NewChannelIndex(PublishImmediately)
	rep := ci.Period("0").AS("0").Rep("v", 1)
	publish := func(n uint64) {
		rep.RestorePublished(SegmentState{SegNo: n, InitPath: fmt.Sprint(n), Discontinuity: true})
	}
	publish(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := uint64(2); n <= 1500; n++ {
			publish(n)
			runtime.Gosched()
		}
	}()
	defer func() { <-done }()
	for i := 0; ; i++ {
		p := ci.RepresentationPeriods("v")[0]
		if len(p.Segments) != 1 || !p.RangeKnown || p.First != 1 {
			t.Fatalf("invalid read: %+v", p)
		}
		n := p.Segments[0].SegNo
		if p.Last != n || p.InitPath != fmt.Sprint(n) || len(p.Discontinuities) != int(n) || p.Discontinuities[len(p.Discontinuities)-1] != n {
			t.Fatalf("fields from different publications: segment=%d last=%d init=%s discontinuities=%d", n, p.Last, p.InitPath, len(p.Discontinuities))
		}
		select {
		case <-done:
			if i >= 100 {
				return
			}
		default:
		}
	}
}

func TestPublishedSegmentPathHonorsPeriodAndPublication(t *testing.T) {
	ci := NewChannelIndex(PublishAVBarrier)
	rep := ci.Period("old").AS("0").Rep("v", 2)
	rep.RestorePublished(SegmentState{SegNo: 1, Path: "evicted"})
	rep.RestorePublished(SegmentState{SegNo: 5, Path: "old.ts", EndPTS: 10, Timescale: 1})
	rep.commit(SegmentState{SegNo: 6, Path: "pending"})
	ci.Period("new").AS("0").Rep("v", 0).RestorePublished(SegmentState{SegNo: 5, Path: "new.m4s"})
	for _, tc := range []struct {
		period string
		n      uint64
		want   string
	}{{"old", 5, "old.ts"}, {"new", 5, "new.m4s"}, {"old", 6, ""}, {"old", 1, ""}, {"missing", 5, ""}, {"old", 7, ""}} {
		if got := ci.PublishedSegmentPath(tc.period, "v", tc.n); got != tc.want {
			t.Errorf("%s/%d = %q, want %q", tc.period, tc.n, got, tc.want)
		}
	}
	rep.ExpireOlderThan(11)
	if ci.PublishedSegmentPath("old", "v", 5) != "" {
		t.Fatal("expired segment is serveable")
	}
}

func TestInitPathPrefersPublishedAndFallsBackToCommitted(t *testing.T) {
	for _, acrossAS := range []bool{false, true} {
		t.Run(fmt.Sprint(acrossAS), func(t *testing.T) {
			ci := NewChannelIndex(PublishAVBarrier)
			rep := ci.Period("0").AS("0").Rep("v", 0)
			rep.RestorePublished(SegmentState{SegNo: 1}) // self-contained published content
			rep.commit(SegmentState{SegNo: 2, InitPath: "pending"})
			if got := ci.InitPath("0", "v"); got != "pending" {
				t.Fatalf("committed fallback = %q", got)
			}
			if got := ci.RepresentationPeriods("v")[0].InitPath; got != "pending" {
				t.Fatalf("period fallback = %q", got)
			}
			published := rep
			if acrossAS {
				published = ci.Period("0").AS("1").Rep("v", 0)
			}
			published.RestorePublished(SegmentState{SegNo: 3, InitPath: "published", EndPTS: 10, Timescale: 1})
			ci.Period("1").AS("0").Rep("v", 0).RestorePublished(SegmentState{SegNo: 3, InitPath: "other-period"})
			for i := 0; i < 30; i++ {
				if got := ci.InitPath("0", "v"); got != "published" {
					t.Fatalf("published priority = %q", got)
				}
				if got := ci.RepresentationPeriods("v")[0].InitPath; got != "published" {
					t.Fatalf("period published priority = %q", got)
				}
			}
			published.ExpireOlderThan(11)
			if got := ci.InitPath("0", "v"); got != "pending" {
				t.Fatalf("expired init leaked = %q", got)
			}
		})
	}
}

func TestReadMissingRepresentationCreatesNothing(t *testing.T) {
	ci := NewChannelIndex(PublishImmediately)
	ci.Period("0").AS("0").Rep("known", 0)
	before := ci.Snapshot()
	if len(ci.RepresentationPeriods("missing")) != 0 || ci.InitPath("missing", "v") != "" || ci.InitPath("0", "missing") != "" || ci.PublishedSegmentPath("missing", "v", 1) != "" || ci.PublishedSegmentPath("0", "missing", 1) != "" {
		t.Fatal("missing read returned content")
	}
	after := ci.Snapshot()
	if !reflect.DeepEqual(before.keys, after.keys) || !reflect.DeepEqual(before.PeriodIDs(), after.PeriodIDs()) {
		t.Fatal("read created an entry")
	}
}

func TestRepresentationPeriodsSortSegments(t *testing.T) {
	ci := NewChannelIndex(PublishImmediately)
	rep := ci.Period("0").AS("0").Rep("v", 0)
	for _, n := range []uint64{8, 3, 5} {
		rep.RestorePublished(SegmentState{SegNo: n, Discontinuity: true})
	}
	p := ci.RepresentationPeriods("v")[0]
	if !slices.Equal(segNos(p.Segments), []uint64{3, 5, 8}) || !slices.Equal(p.Discontinuities, []uint64{3, 5, 8}) {
		t.Fatalf("unsorted read: %+v", p)
	}
	// URLs identify a representation without its AdaptationSet. Preserve the
	// existing merge if two AdaptationSets happen to contain the same ID.
	ci.Period("0").AS("1").Rep("v", 0).RestorePublished(SegmentState{SegNo: 7, Discontinuity: true})
	for i := 0; i < 30; i++ {
		p = ci.RepresentationPeriods("v")[0]
		if !slices.Equal(segNos(p.Segments), []uint64{3, 5, 7, 8}) || !slices.Equal(p.Discontinuities, []uint64{3, 5, 7, 8}) {
			t.Fatalf("unsorted merged read: %+v", p)
		}
	}
}
