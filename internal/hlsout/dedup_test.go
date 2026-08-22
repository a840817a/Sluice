package hlsout

import (
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

func run(segs []index.SegmentState) []uint64 {
	out := make([]uint64, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.SegNo)
	}
	return out
}

// contiguousRun walks the sorted segments looking for a break in numbering. A
// repeated segment number is not a break in coverage, but it fails the
// "next == prev+1" test, which silently truncated the playlist to a couple of
// segments and stalled every player. The index now collapses duplicates before
// they get here; this keeps the output layer from turning a duplicate into an
// outage if anything upstream regresses.
func TestContiguousRunToleratesDuplicates(t *testing.T) {
	segs := []index.SegmentState{
		{SegNo: 5, Status: index.StatusPublished},
		{SegNo: 6, Status: index.StatusPublished},
		{SegNo: 7, Status: index.StatusPublished},
		{SegNo: 7, Status: index.StatusPublished}, // duplicate
		{SegNo: 8, Status: index.StatusPublished},
	}

	t.Run("live keeps the whole trailing run", func(t *testing.T) {
		got := run(contiguousRun(segs, false))
		want := []uint64{5, 6, 7, 8}
		if !equal(got, want) {
			t.Errorf("live run = %v, want %v (a duplicate must not truncate the window)", got, want)
		}
	})

	t.Run("vod keeps the whole leading run", func(t *testing.T) {
		got := run(contiguousRun(segs, true))
		want := []uint64{5, 6, 7, 8}
		if !equal(got, want) {
			t.Errorf("vod run = %v, want %v", got, want)
		}
	})

	t.Run("a real gap still truncates", func(t *testing.T) {
		gapped := []index.SegmentState{
			{SegNo: 5}, {SegNo: 6}, {SegNo: 9}, {SegNo: 10},
		}
		if got, want := run(contiguousRun(gapped, false)), []uint64{9, 10}; !equal(got, want) {
			t.Errorf("live run across a gap = %v, want %v", got, want)
		}
		if got, want := run(contiguousRun(gapped, true)), []uint64{5, 6}; !equal(got, want) {
			t.Errorf("vod run across a gap = %v, want %v", got, want)
		}
	})
}

// The rendered playlist must carry each segment exactly once.
func TestMediaPlaylistHasNoDuplicateSegments(t *testing.T) {
	segs := []index.SegmentState{
		{SegNo: 5, StartPTS: 5 * 90000, EndPTS: 6 * 90000, Timescale: 90000, Status: index.StatusPublished},
		{SegNo: 6, StartPTS: 6 * 90000, EndPTS: 7 * 90000, Timescale: 90000, Status: index.StatusPublished},
		{SegNo: 6, StartPTS: 6 * 90000, EndPTS: 7 * 90000, Timescale: 90000, Status: index.StatusPublished},
		{SegNo: 7, StartPTS: 7 * 90000, EndPTS: 8 * 90000, Timescale: 90000, Status: index.StatusPublished},
	}

	out := string(Media(segs, MediaOptions{
		GatewayBaseURL: "http://gw", ChannelID: "ch", RepID: "v0", TargetDuration: 1,
	}))

	if n := strings.Count(out, "/6.m4s"); n > 1 {
		t.Errorf("segment 6 appears %d times in the playlist:\n%s", n, out)
	}
	for _, want := range []string{"/5.m4s", "/6.m4s", "/7.m4s"} {
		if !strings.Contains(out, want) {
			t.Errorf("playlist missing %s:\n%s", want, out)
		}
	}
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
