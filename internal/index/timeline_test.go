package index

import (
	"fmt"
	"strings"
	"testing"
)

// timelineString renders a Timeline as "period[first..last window discs init]".
func timelineString(tl []TimelinePeriod) string {
	var parts []string
	for _, p := range tl {
		parts = append(parts, fmt.Sprintf("%s[%d..%d %v %v %s]", p.PeriodID, p.First, p.Last, segNos(p.Segments), p.Discontinuities, p.InitPath))
	}
	return strings.Join(parts, " ")
}

func TestTimeline(t *testing.T) {
	published := func(ci *ChannelIndex, period string, nos ...uint64) *RepresentationState {
		rep := ci.Period(period).AS("0").Rep("v", 0)
		for _, n := range nos {
			rep.RestorePublished(SegmentState{SegNo: n, EndPTS: int64(n), Timescale: 1})
		}
		return rep
	}
	for _, tc := range []struct {
		name     string
		build    func(ci *ChannelIndex)
		docOrder []string
		want     string
	}{
		{
			name: "HLS period numbers sort numerically",
			build: func(ci *ChannelIndex) {
				published(ci, "10", 1)
				published(ci, "2", 1)
			},
			want: "2[1..1 [1] [] ] 10[1..1 [1] [] ]",
		},
		{
			name: "DASH periods follow the document",
			build: func(ci *ChannelIndex) {
				published(ci, "a", 1)
				published(ci, "b", 1)
			},
			docOrder: []string{"b", "a"},
			want:     "b[1..1 [1] [] ] a[1..1 [1] [] ]",
		},
		{
			name: "a period that left the document comes first",
			build: func(ci *ChannelIndex) {
				published(ci, "b", 1)
				published(ci, "z", 1)
			},
			docOrder: []string{"b"},
			want:     "z[1..1 [1] [] ] b[1..1 [1] [] ]",
		},
		{
			name: "restarts follow the period they restarted",
			build: func(ci *ChannelIndex) {
				published(ci, "p0.r2", 1)
				published(ci, "p0", 10)
				published(ci, "p0.r1", 1)
			},
			docOrder: []string{"p0.r2"},
			want:     "p0[10..10 [10] [] ] p0.r1[1..1 [1] [] ] p0.r2[1..1 [1] [] ]",
		},
		{
			name: "an emptied window keeps its lifetime range and discontinuities",
			build: func(ci *ChannelIndex) {
				rep := ci.Period("0").AS("0").Rep("v", 0)
				for _, n := range []uint64{100, 101, 102} {
					rep.RestorePublished(SegmentState{SegNo: n, Discontinuity: n == 101, EndPTS: int64(n), Timescale: 1})
				}
				rep.ExpireOlderThan(200)
				published(ci, "1", 0)
			},
			want: "0[100..102 [] [101] ] 1[0..0 [0] [] ]",
		},
		{
			name: "a period with nothing ever published is left out",
			build: func(ci *ChannelIndex) {
				ci.Period("0").AS("0").Rep("v", 2).commit(SegmentState{SegNo: 1, InitPath: "pending"})
				published(ci, "1", 5)
			},
			want: "1[5..5 [5] [] ]",
		},
		{
			name: "other representations are not read",
			build: func(ci *ChannelIndex) {
				ci.Period("0").AS("0").Rep("a", 0).RestorePublished(SegmentState{SegNo: 1})
				published(ci, "1", 2)
			},
			want: "1[2..2 [2] [] ]",
		},
		{
			name: "one ID in two AdaptationSets is one track",
			build: func(ci *ChannelIndex) {
				ci.Period("0").AS("0").Rep("v", 0).RestorePublished(SegmentState{SegNo: 5, InitPath: "init-a"})
				ci.Period("0").AS("1").Rep("v", 0).RestorePublished(SegmentState{SegNo: 3})
			},
			want: "0[3..5 [3 5] [] init-a]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ci := NewChannelIndex(PublishImmediately)
			tc.build(ci)
			if got := timelineString(ci.Timeline("v", tc.docOrder)); got != tc.want {
				t.Errorf("Timeline =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
	if tl := NewChannelIndex(PublishImmediately).Timeline("v", nil); len(tl) != 0 {
		t.Errorf("empty index Timeline = %v", tl)
	}
}
