package channel

import (
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

// internal/channel has no tests of its own; everything it does is reached only
// through the httpapi end-to-end suite. buildStatus is one of the hand-rolled
// channel -> period -> AdaptationSet -> representation walks, so it is exactly
// the kind of code a refactor can quietly zero out — an admin status page that
// reports 0 segments everywhere looks plausible and breaks no other test.

// seg builds a one-second segment at the given segment number.
func seg(segNo uint64) index.SegmentState {
	return index.SegmentState{
		SegNo:     segNo,
		StartPTS:  int64(segNo) * 90000,
		EndPTS:    int64(segNo+1) * 90000,
		Timescale: 90000,
	}
}

// statusIndex builds an index with two periods, mixed statuses:
//
//	p0/AS 0/v0: 2 published, 1 committed
//	p0/AS 1/a0: 1 published, 1 expired
//	p1/AS 0/v0: 1 committed
func statusIndex(t *testing.T) *index.ChannelIndex {
	t.Helper()
	ci := index.NewChannelIndex()

	v0 := ci.Period("p0").AS("0").Rep("v0", 8)
	ci.Period("p0").AS("0").SetMediaType("video")
	v0.RestorePublished(seg(1))
	v0.RestorePublished(seg(2))
	// An audio AdaptationSet that has not received a segment yet holds the A/V
	// barrier, so a commit stays committed and unpublished.
	ci.Period("p0").AS("2").SetMediaType("audio")
	ci.Commit(index.RepRef{PeriodID: "p0", ASID: "0", RepID: "v0"}, 8, seg(3))

	a0 := ci.Period("p0").AS("1").Rep("a0", 8)
	ci.Period("p0").AS("1").SetMediaType("audio")
	a0.RestorePublished(seg(1))
	a0.RestorePublished(seg(2))
	// EndSec of segment 2 is 3.0, so a cutoff of 4.0 expires it. Segment 1 ends
	// at 2.0 and would expire too, so expire against a cutoff that catches only
	// the older one.
	a0.RestorePublished(seg(0))
	a0.ExpireOlderThan(1.5) // segment 0 ends at 1.0

	ci.Period("p1").AS("0").SetMediaType("video")
	ci.Period("p1").AS("1").SetMediaType("audio")
	ci.Commit(index.RepRef{PeriodID: "p1", ASID: "0", RepID: "v0"}, 8, seg(1))

	return ci
}

// byPeriod indexes the result by period ID. buildStatus appends periods in map
// iteration order, which Go randomises, so any assertion keyed on slice
// position is flaky by construction.
func byPeriod(s ChannelStatus) map[string]PeriodStats {
	out := make(map[string]PeriodStats, len(s.Periods))
	for _, p := range s.Periods {
		out[p.PeriodID] = p
	}
	return out
}

func TestBuildStatusCountsEveryPeriodAndRepresentation(t *testing.T) {
	rt := &Runtime{
		ID:    "ch1",
		Index: statusIndex(t),
	}
	rt.setConfig(Config{MPDURL: "http://example.com/live.mpd"})

	s := buildStatus(rt)

	if s.ID != "ch1" {
		t.Errorf("ID = %q, want ch1", s.ID)
	}
	if s.MPDURL != "http://example.com/live.mpd" {
		t.Errorf("MPDURL = %q", s.MPDURL)
	}
	if !s.Running {
		t.Error("Running = false, want true")
	}
	if len(s.Periods) != 2 {
		t.Fatalf("got %d periods, want 2: %+v", len(s.Periods), s.Periods)
	}

	got := byPeriod(s)

	// p0 aggregates both AdaptationSets: video 2 published + 1 committed,
	// audio 2 published + 1 expired.
	if p := got["p0"]; p.Published != 4 || p.Committed != 1 || p.Expired != 1 || p.Total != 6 {
		t.Errorf("p0 = %+v, want published=4 committed=1 expired=1 total=6", p)
	}
	if p := got["p1"]; p.Published != 0 || p.Committed != 1 || p.Expired != 0 || p.Total != 1 {
		t.Errorf("p1 = %+v, want published=0 committed=1 expired=0 total=1", p)
	}
}

func TestBuildStatusModeStrings(t *testing.T) {
	for _, tc := range []struct {
		mode ChannelMode
		want string
	}{
		{ModeLive, "live"},
		{ModeTransitioning, "transitioning"},
		{ModeVOD, "vod"},
		{ModeStaticIngesting, "static_ingesting"},
	} {
		rt := &Runtime{ID: "ch1", Index: index.NewChannelIndex()}
		driveMode(t, rt, tc.mode)
		if got := buildStatus(rt).Mode; got != tc.want {
			t.Errorf("mode %d: got %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// A channel that completed a VOD transition must report counts from the
// full-history VOD index, not from the live index it was built alongside.
func TestBuildStatusUsesVODIndexOnlyInVODMode(t *testing.T) {
	live := index.NewChannelIndex()
	live.Period("p0").AS("0").Rep("v0", 8).RestorePublished(seg(1))

	vod := index.NewChannelIndex()
	vodRep := vod.Period("p0").AS("0").Rep("v0", 8)
	for i := uint64(1); i <= 5; i++ {
		vodRep.RestorePublished(seg(i))
	}

	rt := &Runtime{ID: "ch1", Index: live}
	rt.vodIndex.Store(vod)

	// Still live: the VOD index exists but must be ignored.
	if p := byPeriod(buildStatus(rt))["p0"]; p.Published != 1 {
		t.Errorf("live mode published = %d, want 1 (from the live index)", p.Published)
	}

	// ModeStaticIngesting also reads the live index while segments download.
	driveMode(t, rt, ModeStaticIngesting)
	if p := byPeriod(buildStatus(rt))["p0"]; p.Published != 1 {
		t.Errorf("static_ingesting published = %d, want 1 (from the live index)", p.Published)
	}

	driveMode(t, rt, ModeVOD)
	if p := byPeriod(buildStatus(rt))["p0"]; p.Published != 5 {
		t.Errorf("vod mode published = %d, want 5 (from the VOD index)", p.Published)
	}
}

// VOD mode with no completed transition must fall back to the live index
// rather than panicking on a nil index.
func TestBuildStatusVODModeWithoutVODIndex(t *testing.T) {
	live := index.NewChannelIndex()
	live.Period("p0").AS("0").Rep("v0", 8).RestorePublished(seg(1))

	rt := &Runtime{ID: "ch1", Index: live}
	driveMode(t, rt, ModeVOD)

	if p := byPeriod(buildStatus(rt))["p0"]; p.Published != 1 {
		t.Errorf("published = %d, want 1 (fallback to the live index)", p.Published)
	}
}

// Serving a manifest creates period state before any segment commits, so a
// period with no AdaptationSets is reachable and must still be reported — with
// zero counts rather than being dropped from the response.
func TestBuildStatusIncludesPeriodsWithNoAdaptationSets(t *testing.T) {
	ci := index.NewChannelIndex()
	ci.Period("p0").AS("0").Rep("v0", 8).RestorePublished(seg(1))
	ci.Period("p-empty") // created, never populated

	got := byPeriod(buildStatus(&Runtime{ID: "ch1", Index: ci}))
	if len(got) != 2 {
		t.Fatalf("got %d periods, want 2: %v", len(got), got)
	}
	if p, ok := got["p-empty"]; !ok {
		t.Error("period with no AdaptationSets was dropped from the status")
	} else if p.Published != 0 || p.Committed != 0 || p.Expired != 0 || p.Total != 0 {
		t.Errorf("p-empty = %+v, want all zeroes", p)
	}
}

func TestBuildStatusEmptyIndex(t *testing.T) {
	s := buildStatus(&Runtime{ID: "ch1", Index: index.NewChannelIndex()})
	if len(s.Periods) != 0 {
		t.Errorf("got %d periods for an empty index, want 0", len(s.Periods))
	}
	if !s.Running || s.Mode != "live" {
		t.Errorf("unexpected status for an empty index: %+v", s)
	}
}

// Manager.Channels reports every configured channel: a running one with its
// live status, a stopped one as not running.
func TestManagerChannelsListsEveryConfiguredChannel(t *testing.T) {
	m := newTestManager(t, configForTest(t))

	if got := m.Channels(); len(got) != 0 {
		t.Fatalf("empty manager reported %d channels", len(got))
	}

	for id, url := range map[string]string{"ch1": "u1", "ch2": "u2", "ch3": "u3"} {
		if err := m.store.Put(id, Config{MPDURL: url}); err != nil {
			t.Fatal(err)
		}
	}
	rt1 := &Runtime{ID: "ch1", Index: statusIndex(t)}
	rt1.setConfig(Config{MPDURL: "u1"})
	rt2 := &Runtime{ID: "ch2", Index: index.NewChannelIndex()}
	rt2.setConfig(Config{MPDURL: "u2"})

	m.mu.Lock()
	m.runtimes["ch1"] = rt1
	m.runtimes["ch2"] = rt2
	m.mu.Unlock()

	got := m.Channels()
	if len(got) != 3 {
		t.Fatalf("got %d channels, want 3", len(got))
	}
	if c, ok := got["ch1"]; !ok || !c.Status.Running || c.Status.MPDURL != "u1" || len(c.Status.Periods) != 2 {
		t.Errorf("ch1 = %+v", c)
	}
	if c, ok := got["ch2"]; !ok || !c.Status.Running || c.Status.MPDURL != "u2" || len(c.Status.Periods) != 0 {
		t.Errorf("ch2 = %+v", c)
	}
	if c, ok := got["ch3"]; !ok || c.Status.Running || c.Status.ID != "ch3" || c.Status.MPDURL != "u3" || c.Config.MPDURL != "u3" {
		t.Errorf("ch3, configured but stopped = %+v", c)
	}
}
