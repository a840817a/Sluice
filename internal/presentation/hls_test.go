package presentation

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// The playlist numbers a later period after an earlier one's lifetime range,
// so the renderer must pass that range on rather than let it be recomputed
// from whatever the ring still holds, and must order periods numerically. The
// same holds for the lifetime record of origin discontinuities, which
// EXT-X-DISCONTINUITY-SEQUENCE counts.
func TestPeriodSegmentsCarryLifetimeRangeInOrder(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	for _, period := range []string{"10", "2"} {
		rep := ci.Period(period).AS("0").Rep("v0", 2) // holds 2; 5 published
		for n := uint64(100); n <= 104; n++ {
			rep.RestorePublished(index.SegmentState{SegNo: n, EndPTS: 1, Timescale: 1, Discontinuity: n == 101})
		}
	}
	got := testGenerator().periodSegments(Channel{ID: "c", Index: ci}, "v0")
	if len(got) != 2 || got[0].PeriodID != "2" || got[1].PeriodID != "10" {
		t.Fatalf("periods = %+v, want 2 then 10", got)
	}
	for _, p := range got {
		if !p.RangeKnown || p.First != 100 || p.Last != 104 {
			t.Errorf("period %s range = %d..%d (known %v), want the lifetime 100..104", p.PeriodID, p.First, p.Last, p.RangeKnown)
		}
		if !slices.Equal(p.Discontinuities, []uint64{101}) {
			t.Errorf("period %s discontinuities = %v, want the evicted 101", p.PeriodID, p.Discontinuities)
		}
	}
}

// A segment the index committed but never published — a video segment whose
// audio never arrived before an origin reset, say — is not part of what the
// playlist can list. It must not open a gap in the media sequence at a period
// boundary, or the playlist cuts at the gap and loses whole periods.
//
// The index is built the way ingest builds it, through Commit under the A/V
// barrier, because that is where the committed-only segment comes from: once
// an audio AdaptationSet is known but has no segment, the barrier holds every
// later video segment back.
func TestUnpublishedSegmentAtAPeriodEdgeKeepsBothPeriods(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	commit := func(period string, first, last, publishedTo uint64) {
		for n := first; n <= last; n++ {
			if n == publishedTo+1 {
				ci.Period(period).AS("1").SetMediaType(index.MediaAudio)
			}
			ci.Commit(index.RepRef{PeriodID: period, ASID: "0", RepID: "v0", MediaType: index.MediaVideo}, 100,
				index.SegmentState{SegNo: n, StartPTS: int64(n) * 90000, EndPTS: int64(n+1) * 90000, Timescale: 90000, Path: "/x/" + period})
		}
	}
	commit("0", 1, 5, 4) // segment 5 committed, never published
	commit("1", 1, 3, 3)

	periods := testGenerator().periodSegments(Channel{ID: "c", Index: ci}, "v0")
	want := []string{"0/v0/1", "0/v0/2", "0/v0/3", "0/v0/4", "1/v0/1", "1/v0/2", "1/v0/3"}
	for _, vod := range []bool{true, false} {
		out := string(hlsout.Media(periods, hlsout.MediaOptions{GatewayBaseURL: "http://gw", ChannelID: "c", RepID: "v0", VOD: vod, TargetDuration: 1}))
		var got []string
		for _, m := range regexp.MustCompile(`/segments/(\S+)\.m4s`).FindAllStringSubmatch(out, -1) {
			got = append(got, m[1])
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("VOD=%v lists %v, want both periods' published segments %v\n%s", vod, got, want, out)
		}
	}
}

func TestHLSReadPreservesDocumentOrderAndInitURLs(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	for _, id := range []string{"2", "10"} {
		ci.Period(id).AS("1").SetMediaType(index.MediaAudio)
		ci.Commit(index.RepRef{PeriodID: id, ASID: "0", RepID: "v", MediaType: index.MediaVideo}, 0, index.SegmentState{SegNo: 2, InitPath: "pending"})
		ci.FindRep(id, "0", "v").RestorePublished(index.SegmentState{SegNo: 1, InitPath: "published", EndPTS: 1, Timescale: 1})
	}
	ch := Channel{ID: "c", Index: ci, ParsedMPD: &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{ID: "10"}, {ID: "2"}}}}
	got := testGenerator().periodSegments(ch, "v")
	if len(got) != 2 || got[0].PeriodID != "10" || got[1].PeriodID != "2" {
		t.Fatalf("DASH document order: %+v", got)
	}
	for _, p := range got {
		if p.InitURI != gwurl.InitURL("http://gw.test", "c", p.PeriodID, "v") || len(p.Segments) != 1 || p.Segments[0].SegNo != 1 || !p.RangeKnown || p.First != 1 || p.Last != 1 {
			t.Fatalf("HLS read: %+v", p)
		}
	}
}

func TestMasterRefusesWhatTheChannelCannotServe(t *testing.T) {
	g := testGenerator()
	encrypted := Channel{ID: "c1", Tracks: Tracks{ServesDASH: true}}
	if _, err := g.Master(encrypted); !errors.Is(err, ErrNotServed) {
		t.Errorf("encrypted DASH source: Master error = %v, want ErrNotServed", err)
	}
	undiscovered := Channel{ID: "c1", Tracks: Tracks{ServesHLS: true, ServesDASH: true}}
	if _, err := g.Master(undiscovered); !errors.Is(err, ErrNotReady) {
		t.Errorf("no tracks yet: Master error = %v, want ErrNotReady", err)
	}
	out, err := g.Master(fmp4HLSChannel(hlsIndex("v0", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "http://gw.test/v1/channels/c1/media/v0.m3u8") {
		t.Errorf("master does not point at v0's media playlist:\n%s", out)
	}
}

func TestMediaRefusesWhatTheChannelCannotServe(t *testing.T) {
	g := testGenerator()
	if _, err := g.Media(Channel{ID: "c1", Tracks: Tracks{ServesDASH: true}}, "v0"); !errors.Is(err, ErrNotServed) {
		t.Errorf("encrypted DASH source: Media error = %v, want ErrNotServed", err)
	}
	if _, err := g.Media(fmp4HLSChannel(hlsIndex("v0", 1)), "nope"); !errors.Is(err, ErrNotServed) {
		t.Errorf("unknown representation: Media error = %v, want ErrNotServed", err)
	}
}

func segmentCount(playlist []byte) int {
	return strings.Count(string(playlist), "#EXTINF:")
}

// A live playlist lists the configured window — 120s of 4s segments — while a
// channel that keeps everything, or has become VOD, lists every segment.
func TestMediaWindow(t *testing.T) {
	g := testGenerator()
	ch := fmp4HLSChannel(hlsIndex("v0", 40))

	for _, tc := range []struct {
		name         string
		keepAll, vod bool
		want         int
		wantEndList  bool
	}{
		{"live", false, false, 30, false},
		{"keep all", true, false, 40, false},
		{"VOD", false, true, 40, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch.KeepAll, ch.VOD = tc.keepAll, tc.vod
			out, err := g.Media(ch, "v0")
			if err != nil {
				t.Fatal(err)
			}
			if got := segmentCount(out); got != tc.want {
				t.Errorf("lists %d segments, want %d", got, tc.want)
			}
			if got := strings.Contains(string(out), "#EXT-X-ENDLIST"); got != tc.wantEndList {
				t.Errorf("ENDLIST = %v, want %v", got, tc.wantEndList)
			}
		})
	}
}

// RFC 8216 forbids a playlist's target duration from shrinking between
// reloads, so once a long segment has been advertised the target stays.
func TestMediaTargetDurationNeverShrinks(t *testing.T) {
	g := testGenerator()
	long := index.NewChannelIndex(index.PublishAVBarrier)
	as := long.Period("0").AS("0")
	as.SetMediaType(index.MediaVideo)
	as.Rep("v0", 0).RestorePublished(index.SegmentState{SegNo: 0, EndPTS: 10000, Timescale: 1000, Path: "/d/0.m4s", InitPath: "/d/init.mp4"})

	first, err := g.Media(fmp4HLSChannel(long), "v0")
	if err != nil {
		t.Fatal(err)
	}
	later, err := g.Media(fmp4HLSChannel(hlsIndex("v0", 3)), "v0")
	if err != nil {
		t.Fatal(err)
	}
	td := regexp.MustCompile(`#EXT-X-TARGETDURATION:(\d+)`)
	if a, b := td.FindSubmatch(first), td.FindSubmatch(later); a == nil || b == nil || string(a[1]) != "10" || string(b[1]) != "10" {
		t.Errorf("target durations %q then %q, want 10 both times", a, b)
	}
}
