package hlsout

import (
	"regexp"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

// segRange builds consecutive one-second TS segments first..last.
func segRange(first, last uint64) []index.SegmentState {
	var out []index.SegmentState
	for n := first; n <= last; n++ {
		out = append(out, seg(n, float64(n), 1))
	}
	return out
}

// playlistSegments returns the segment URI paths a playlist lists, in order.
func playlistSegments(out string) []string {
	return regexp.MustCompile(`(?m)/segments/(\S+)$`).FindAllString(out, -1)
}

func tagValue(t *testing.T, out, tag string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + tag + `:(\S+)$`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// An upstream reset rewinds the numbering: the new period's segments are
// numbered after the old period's, not below them, and the boundary carries a
// discontinuity and the new period's init.
func TestMediaNumbersARewoundPeriodAfterThePreviousOne(t *testing.T) {
	periods := []PeriodSegments{
		{PeriodID: "0", Segments: segRange(100, 104), InitURI: "https://gw/init/0/v0.mp4"},
		{PeriodID: "1", Segments: segRange(0, 2), InitURI: "https://gw/init/1/v0.mp4"},
	}
	out := string(Media(periods, mediaOpts()))

	if got := tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE"); got != "100" {
		t.Errorf("media sequence = %s, want 100", got)
	}
	want := []string{
		"/segments/0/v0/100.ts", "/segments/0/v0/101.ts", "/segments/0/v0/102.ts", "/segments/0/v0/103.ts", "/segments/0/v0/104.ts",
		"/segments/1/v0/0.ts", "/segments/1/v0/1.ts", "/segments/1/v0/2.ts",
	}
	if got := playlistSegments(out); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("segments =\n%v\nwant\n%v", got, want)
	}
	boundary := out[strings.Index(out, "/segments/0/v0/104.ts"):strings.Index(out, "/segments/1/v0/0.ts")]
	if !strings.Contains(boundary, "#EXT-X-DISCONTINUITY\n") || !strings.Contains(boundary, `#EXT-X-MAP:URI="https://gw/init/1/v0.mp4"`) {
		t.Errorf("the period boundary lacks a discontinuity or the new init:\n%s", boundary)
	}
	if strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 1 || strings.Count(out, "#EXT-X-MAP:") != 2 {
		t.Errorf("want one discontinuity and one map per period:\n%s", out)
	}
}

// A live window sliding across a period boundary must not renumber what it
// still shows: the new period's numbers come from the old period's lifetime
// range, not from whichever of its segments are still published.
func TestMediaSequenceStableWhileTheWindowSlides(t *testing.T) {
	opts := mediaOpts()
	opts.WindowSegments = 3
	render := func(oldPublished []index.SegmentState) string {
		return string(Media([]PeriodSegments{
			{PeriodID: "0", Segments: oldPublished, First: 100, Last: 104, RangeKnown: true},
			{PeriodID: "1", Segments: segRange(0, 2), First: 0, Last: 2, RangeKnown: true},
		}, opts))
	}
	for _, tc := range []struct {
		name string
		old  []index.SegmentState
	}{
		{"old period fully published", segRange(100, 104)},
		{"old period mostly expired", segRange(104, 104)},
		{"old period gone from the window", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := render(tc.old)
			if got := tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE"); got != "105" {
				t.Errorf("media sequence = %s, want 105 (the new period's first segment)\n%s", got, out)
			}
			if got := tagValue(t, out, "#EXT-X-DISCONTINUITY-SEQUENCE"); got != "1" {
				t.Errorf("discontinuity sequence = %q, want 1: the boundary has left the window\n%s", got, out)
			}
		})
	}

	// One segment wider and the boundary is back inside the window.
	opts.WindowSegments = 4
	out := render(segRange(100, 104))
	if tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE") != "104" || strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE") ||
		strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Errorf("window holding the boundary is misnumbered:\n%s", out)
	}
}

// A VOD lists every period, oldest first.
func TestMediaVODListsEveryPeriod(t *testing.T) {
	opts := mediaOpts()
	opts.VOD = true
	out := string(Media([]PeriodSegments{
		{PeriodID: "0", Segments: segRange(100, 104)},
		{PeriodID: "1", Segments: segRange(0, 2)},
	}, opts))
	if n := len(playlistSegments(out)); n != 8 {
		t.Errorf("VOD lists %d segments, want all 8:\n%s", n, out)
	}
	if !strings.Contains(out, "#EXT-X-ENDLIST") || tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE") != "100" {
		t.Errorf("VOD playlist malformed:\n%s", out)
	}
}

// A multi-period DASH source that continues its numbering keeps its numbers.
func TestMediaKeepsNumberingThatContinuesAcrossPeriods(t *testing.T) {
	out := string(Media([]PeriodSegments{
		{PeriodID: "a", Segments: segRange(0, 4)},
		{PeriodID: "b", Segments: segRange(5, 9)},
	}, mediaOpts()))
	if got := tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE"); got != "0" {
		t.Errorf("media sequence = %s, want 0", got)
	}
	if got := playlistSegments(out); len(got) != 10 || got[9] != "/segments/b/v0/9.ts" {
		t.Errorf("segments = %v", got)
	}
}
