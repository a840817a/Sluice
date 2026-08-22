package manifest

import (
	"reflect"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

func TestBuildTimelineDoesNotCollapseGaps(t *testing.T) {
	segs := []index.SegmentState{
		{SegNo: 1, StartPTS: 0, EndPTS: 2, Timescale: 1},
		{SegNo: 2, StartPTS: 2, EndPTS: 4, Timescale: 1},
		{SegNo: 4, StartPTS: 6, EndPTS: 8, Timescale: 1},
	}

	tl := buildTimeline(segs)
	if tl == nil {
		t.Fatal("expected timeline")
	}
	if len(tl.S) != 2 {
		t.Fatalf("expected 2 timeline entries, got %d", len(tl.S))
	}
	if tl.S[0].R == nil || *tl.S[0].R != 1 {
		t.Fatalf("expected first entry repeat count 1, got %#v", tl.S[0].R)
	}
	if tl.S[1].T == nil || *tl.S[1].T != 6 {
		t.Fatalf("expected second entry to start at 6, got %#v", tl.S[1].T)
	}
}

func TestProjectPublishedSegmentsAppliesWindowAndSafeEdge(t *testing.T) {
	cfg := GeneratorConfig{
		WindowDepth:    4 * time.Second,
		SafeEdgeBuffer: 2 * time.Second,
	}
	segs := []index.SegmentState{
		{SegNo: 1, StartPTS: 0, EndPTS: 2, Timescale: 1},
		{SegNo: 2, StartPTS: 2, EndPTS: 4, Timescale: 1},
		{SegNo: 3, StartPTS: 4, EndPTS: 6, Timescale: 1},
		{SegNo: 4, StartPTS: 6, EndPTS: 8, Timescale: 1},
		{SegNo: 5, StartPTS: 8, EndPTS: 10, Timescale: 1},
	}

	got := projectPublishedSegments(imdp.PresentationDynamic, segs, cfg)
	var gotNos []uint64
	for _, seg := range got {
		gotNos = append(gotNos, seg.SegNo)
	}
	want := []uint64{3, 4}
	if !reflect.DeepEqual(gotNos, want) {
		t.Fatalf("expected segment numbers %v, got %v", want, gotNos)
	}
}

func TestDynamicMinimumUpdatePeriodFallback(t *testing.T) {
	if got := dynamicMinimumUpdatePeriod(3*time.Second, 5*time.Second); got != 3*time.Second {
		t.Fatalf("expected upstream MUP to win, got %s", got)
	}
	if got := dynamicMinimumUpdatePeriod(0, 5*time.Second); got != 5*time.Second {
		t.Fatalf("expected fallback MUP, got %s", got)
	}
	if got := dynamicMinimumUpdatePeriod(0, 0); got != 2*time.Second {
		t.Fatalf("expected default MUP, got %s", got)
	}
}
