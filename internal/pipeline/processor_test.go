package pipeline

import (
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
)

func TestExpireOldSegments_UsesLiveEdgeWindow(t *testing.T) {
	ci := index.NewChannelIndex()
	ps := ci.Period("p0")
	as := ps.AS("v0")
	as.SetMediaType("video")
	rep := as.Rep("r0", 16)

	// EndSec: 10, 20, 30 (timescale=10)
	rep.Commit(index.SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 100, Timescale: 10})
	rep.Commit(index.SegmentState{SegNo: 2, StartPTS: 100, EndPTS: 200, Timescale: 10})
	rep.Commit(index.SegmentState{SegNo: 3, StartPTS: 200, EndPTS: 300, Timescale: 10})
	rep.MarkPublished(map[uint64]struct{}{1: {}, 2: {}, 3: {}})

	p := NewProcessor(config.Config{
		Window: config.WindowConfig{
			Depth:          12 * time.Second,
			SafeEdgeBuffer: 3 * time.Second,
		},
	}, "ch1", ci, nil)

	// live edge = 30s, cutoff = 30 - (12+3) = 15s
	// => seg#1 expires (EndSec=10), seg#2/#3 stay published.
	p.expireOldSegments(ps)

	published, committed, expired := rep.StatusCounts()
	if committed != 0 {
		t.Fatalf("committed=%d, want 0", committed)
	}
	if published != 2 {
		t.Fatalf("published=%d, want 2", published)
	}
	if expired != 1 {
		t.Fatalf("expired=%d, want 1", expired)
	}
}

func TestExpireOldSegments_NoPublished_NoChange(t *testing.T) {
	ci := index.NewChannelIndex()
	ps := ci.Period("p0")
	as := ps.AS("v0")
	as.SetMediaType("video")
	rep := as.Rep("r0", 8)

	rep.Commit(index.SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 100, Timescale: 10})

	p := NewProcessor(config.Config{
		Window: config.WindowConfig{
			Depth:          10 * time.Second,
			SafeEdgeBuffer: 2 * time.Second,
		},
	}, "ch1", ci, nil)

	p.expireOldSegments(ps)

	published, committed, expired := rep.StatusCounts()
	if published != 0 || committed != 1 || expired != 0 {
		t.Fatalf("counts changed unexpectedly: published=%d committed=%d expired=%d", published, committed, expired)
	}
}
