package dashingest

import (
	"testing"
	"time"

	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Segment counts are derived entirely from upstream manifest numbers. A manifest
// that asks for billions of segments must be truncated rather than allocated,
// otherwise one bad channel OOMs the process and takes every other channel with
// it. These cases all complete instantly when the cap holds and hang or exhaust
// memory when it does not.
//
// Each case also pins the truncation flag, which is what tells the admin UI that
// a segment total is a floor and that a percentage or ETA derived from it would
// be wrong — and wrong in the reassuring direction.
func TestSegmentNumbersAreBounded(t *testing.T) {
	t.Run("absurd timeShiftBufferDepth on a live stream", func(t *testing.T) {
		p := &imdp.ParsedMPD{
			Type:                  imdp.PresentationDynamic,
			AvailabilityStartTime: time.Now().Add(-24 * time.Hour),
			TimeShiftBufferDepth:  100_000 * time.Hour,
		}
		period := &imdp.ParsedPeriod{}
		tmpl := &imdp.ParsedSegmentTemplate{
			Timescale: 1, Duration: 1, StartNumber: 1, Media: "$Number$.m4s",
		}

		refs, truncated := segmentNumbers(p, period, tmpl)
		if uint64(len(refs)) > maxSegmentsPerPass {
			t.Errorf("got %d segments, want at most %d", len(refs), maxSegmentsPerPass)
		}
		// This case is held down by elapsed wall-clock time, not by the cap:
		// liveTimelineCount derives the count from now-AvailabilityStartTime
		// (~86,400 one-second segments for a 24h-old AST) and the huge
		// timeShiftBufferDepth only ever clamps that number downwards. So the cap
		// is never reached and the total is exact — a live stream gets no total
		// anyway, but flagging it capped here would be a false positive.
		if truncated {
			t.Error("truncated = true, but the elapsed-time bound applied, not the cap")
		}
	})

	t.Run("static period long enough to overflow memory", func(t *testing.T) {
		p := &imdp.ParsedMPD{Type: imdp.PresentationStatic}
		period := &imdp.ParsedPeriod{Duration: 1_000_000 * time.Hour}
		tmpl := &imdp.ParsedSegmentTemplate{
			Timescale: 90000, Duration: 90000, StartNumber: 1, Media: "$Number$.m4s",
		}

		refs, truncated := segmentNumbers(p, period, tmpl)
		if uint64(len(refs)) > maxSegmentsPerPass {
			t.Errorf("got %d segments, want at most %d", len(refs), maxSegmentsPerPass)
		}
		if !truncated {
			t.Error("truncated = false, want true when the cap was applied")
		}
	})

	t.Run("startNumber 0 with a sub-tick period does not run away", func(t *testing.T) {
		p := &imdp.ParsedMPD{Type: imdp.PresentationStatic}
		period := &imdp.ParsedPeriod{Duration: time.Microsecond}
		tmpl := &imdp.ParsedSegmentTemplate{
			Timescale: 1000, Duration: 1, StartNumber: 0, Media: "$Number$.m4s",
		}

		refs, truncated := segmentNumbers(p, period, tmpl)
		if len(refs) != 0 {
			t.Errorf("got %d segments for an empty period, want 0", len(refs))
		}
		// An empty period is not a truncated one: there was nothing to cut.
		if truncated {
			t.Error("truncated = true for an empty period, want false")
		}
	})

	t.Run("SegmentTimeline with a huge repeat count", func(t *testing.T) {
		p := &imdp.ParsedMPD{Type: imdp.PresentationStatic}
		period := &imdp.ParsedPeriod{}
		tmpl := &imdp.ParsedSegmentTemplate{
			Timescale: 90000, StartNumber: 1, Media: "$Time$.m4s",
			Timeline: []imdp.SegmentTimelineEntry{
				{T: 0, D: 90000, R: 100_000_000_000},
			},
		}

		refs, truncated := segmentNumbers(p, period, tmpl)
		if uint64(len(refs)) > maxSegmentsPerPass {
			t.Errorf("got %d segments, want at most %d", len(refs), maxSegmentsPerPass)
		}
		if !truncated {
			t.Error("truncated = false, want true when the cap was applied")
		}
	})

	t.Run("repeat counts summed across entries stay capped", func(t *testing.T) {
		p := &imdp.ParsedMPD{Type: imdp.PresentationStatic}
		period := &imdp.ParsedPeriod{}
		tl := make([]imdp.SegmentTimelineEntry, 50)
		for i := range tl {
			tl[i] = imdp.SegmentTimelineEntry{D: 90000, R: 1_000_000}
		}
		tmpl := &imdp.ParsedSegmentTemplate{
			Timescale: 90000, StartNumber: 1, Media: "$Time$.m4s", Timeline: tl,
		}

		refs, truncated := segmentNumbers(p, period, tmpl)
		if uint64(len(refs)) > maxSegmentsPerPass {
			t.Errorf("got %d segments, want at most %d", len(refs), maxSegmentsPerPass)
		}
		if !truncated {
			t.Error("truncated = false, want true when the cap was applied")
		}
	})
}

// A normal manifest must be entirely unaffected by the cap. This is the case that
// matters most for the progress UI: if an ordinary asset were flagged as
// truncated, every channel would silently lose its percentage and its ETA.
func TestSegmentNumbersNormalManifestUncapped(t *testing.T) {
	p := &imdp.ParsedMPD{Type: imdp.PresentationStatic}
	period := &imdp.ParsedPeriod{Duration: 40 * time.Second}
	tmpl := &imdp.ParsedSegmentTemplate{
		Timescale: 90000, Duration: 4 * 90000, StartNumber: 1, Media: "$Number$.m4s",
	}

	refs, truncated := segmentNumbers(p, period, tmpl)
	if len(refs) != 10 {
		t.Fatalf("got %d segments, want 10", len(refs))
	}
	if truncated {
		t.Error("truncated = true for a 10-segment manifest, want false")
	}
	for i, r := range refs {
		if r.segNo != uint64(i+1) {
			t.Errorf("refs[%d].segNo = %d, want %d", i, r.segNo, i+1)
		}
	}
}
