package index

import (
	"testing"
)

// seg builds a published-or-not segment ending at endSec (timescale 10).
func expSeg(segNo uint64, endSec float64, path string) SegmentState {
	return SegmentState{
		SegNo:     segNo,
		StartPTS:  int64(endSec*10) - 10,
		EndPTS:    int64(endSec * 10),
		Timescale: 10,
		Path:      path,
	}
}

func newRepWith(t *testing.T, segs ...SegmentState) *RepresentationState {
	t.Helper()
	rep := NewChannelIndex().Period("p0").AS("v0").Rep("r0", 16)
	published := map[uint64]struct{}{}
	for _, s := range segs {
		rep.Commit(s)
		published[s.SegNo] = struct{}{}
	}
	rep.MarkPublished(published)
	return rep
}

// The returned paths are a delete list: every entry names a file the caller is
// about to remove. Anything wrongly included is data loss, so the membership
// rule is worth pinning precisely.
func TestExpireAndCollectPathsReturnsOnlyWhatItExpired(t *testing.T) {
	rep := newRepWith(t,
		expSeg(1, 10, "/data/ch/1.m4s"),
		expSeg(2, 20, "/data/ch/2.m4s"),
		expSeg(3, 30, "/data/ch/3.m4s"),
	)

	got := rep.ExpireAndCollectPaths(25)

	want := []string{"/data/ch/1.m4s", "/data/ch/2.m4s"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	published, _, expired := rep.StatusCounts()
	if published != 1 || expired != 2 {
		t.Errorf("published=%d expired=%d, want 1 and 2", published, expired)
	}
}

// Calling twice must not hand back the same files again. Nothing would break
// today — the caller ignores IsNotExist — but "already expired" is the guard
// that keeps the delete list meaning "delete this now".
func TestExpireAndCollectPathsIsNotRepeatable(t *testing.T) {
	rep := newRepWith(t,
		expSeg(1, 10, "/data/ch/1.m4s"),
		expSeg(2, 20, "/data/ch/2.m4s"),
	)

	if first := rep.ExpireAndCollectPaths(25); len(first) != 2 {
		t.Fatalf("first call returned %v, want 2 paths", first)
	}
	if second := rep.ExpireAndCollectPaths(25); len(second) != 0 {
		t.Errorf("second call returned %v, want nothing: those files are already gone", second)
	}
}

// A segment still waiting on the A/V barrier has been written to disk but is not
// yet serveable. Expiry works on the published window, so committed segments
// must not be swept out from under it.
func TestExpireAndCollectPathsLeavesCommittedSegments(t *testing.T) {
	rep := NewChannelIndex().Period("p0").AS("v0").Rep("r0", 16)
	rep.Commit(expSeg(1, 10, "/data/ch/1.m4s")) // committed, never published

	got := rep.ExpireAndCollectPaths(25)

	if len(got) != 0 {
		t.Errorf("paths = %v, want none: the segment was never published", got)
	}
	_, committed, expired := rep.StatusCounts()
	if committed != 1 || expired != 0 {
		t.Errorf("committed=%d expired=%d, want 1 and 0", committed, expired)
	}
}

// A segment with no recorded path still has to leave the window; it just has
// nothing to delete. Returning "" would make the caller remove the process's
// working directory entry, or log a confusing failure.
func TestExpireAndCollectPathsSkipsEmptyPathsButStillExpires(t *testing.T) {
	rep := newRepWith(t,
		expSeg(1, 10, ""),
		expSeg(2, 20, "/data/ch/2.m4s"),
	)

	got := rep.ExpireAndCollectPaths(25)

	if len(got) != 1 || got[0] != "/data/ch/2.m4s" {
		t.Errorf("paths = %v, want only the segment that has one", got)
	}
	published, _, expired := rep.StatusCounts()
	if published != 0 || expired != 2 {
		t.Errorf("published=%d expired=%d, want 0 and 2: both segments left the window", published, expired)
	}
}

// The boundary is strict: a segment ending exactly at the cutoff is still inside
// the window. Off by one here deletes a segment a player may still be asking for.
func TestExpireAndCollectPathsCutoffIsExclusive(t *testing.T) {
	rep := newRepWith(t, expSeg(1, 20, "/data/ch/1.m4s"))

	if got := rep.ExpireAndCollectPaths(20); len(got) != 0 {
		t.Errorf("paths = %v, want none: a segment ending exactly at the cutoff is still in the window", got)
	}
	if got := rep.ExpireAndCollectPaths(20.001); len(got) != 1 {
		t.Errorf("paths = %v, want one: the segment is now behind the cutoff", got)
	}
}

// ExpireOlderThan is the same decision without the delete list — the two must
// agree on which segments leave the window, or the two cleanup modes would
// serve different content.
func TestExpireOlderThanMatchesExpireAndCollectPaths(t *testing.T) {
	segs := []SegmentState{
		expSeg(1, 10, "/data/ch/1.m4s"),
		expSeg(2, 20, "/data/ch/2.m4s"),
		expSeg(3, 30, "/data/ch/3.m4s"),
	}

	withPaths := newRepWith(t, segs...)
	withPaths.ExpireAndCollectPaths(25)
	wantPub, wantCom, wantExp := withPaths.StatusCounts()

	indexOnly := newRepWith(t, segs...)
	indexOnly.ExpireOlderThan(25)
	gotPub, gotCom, gotExp := indexOnly.StatusCounts()

	if gotPub != wantPub || gotCom != wantCom || gotExp != wantExp {
		t.Errorf("ExpireOlderThan gave published=%d committed=%d expired=%d, "+
			"but ExpireAndCollectPaths gave %d/%d/%d — the two cleanup modes disagree "+
			"about what is still in the window",
			gotPub, gotCom, gotExp, wantPub, wantCom, wantExp)
	}
}
