package hlsout

import (
	"regexp"
	"strconv"
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

// An origin reset rewinds the numbering: the new period's segments are
// numbered after the old period's, not below them, and the boundary carries a
// discontinuity and the new period's init.
func TestMediaNumbersARewoundPeriodAfterThePreviousOne(t *testing.T) {
	withInit := func(segs []index.SegmentState) []index.SegmentState {
		for i := range segs {
			segs[i].InitPath = "init.mp4"
		}
		return segs
	}
	periods := timeline(
		testPeriod{id: "0", segs: withInit(segRange(100, 104))},
		testPeriod{id: "1", segs: withInit(segRange(0, 2))},
	)
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
	if !strings.Contains(boundary, "#EXT-X-DISCONTINUITY\n") || !strings.Contains(boundary, `#EXT-X-MAP:URI="https://gw.example.com/v1/channels/ch1/init/1/v0.mp4"`) {
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
	// The old period published 100..104; those below oldPresent have left
	// the window.
	render := func(oldPresent uint64) string {
		return string(Media(timeline(
			testPeriod{id: "0", segs: segRange(100, 104), present: oldPresent},
			testPeriod{id: "1", segs: segRange(0, 2)},
		), opts))
	}
	for _, tc := range []struct {
		name       string
		oldPresent uint64
	}{
		{"old period fully published", 100},
		{"old period mostly expired", 104},
		{"old period gone from the window", 105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := render(tc.oldPresent)
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
	out := render(100)
	if tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE") != "104" || strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE") ||
		strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Errorf("window holding the boundary is misnumbered:\n%s", out)
	}
}

// A VOD lists every period, oldest first.
func TestMediaVODListsEveryPeriod(t *testing.T) {
	opts := mediaOpts()
	opts.VOD = true
	out := string(Media(timeline(
		testPeriod{id: "0", segs: segRange(100, 104)},
		testPeriod{id: "1", segs: segRange(0, 2)},
	), opts))
	if n := len(playlistSegments(out)); n != 8 {
		t.Errorf("VOD lists %d segments, want all 8:\n%s", n, out)
	}
	if !strings.Contains(out, "#EXT-X-ENDLIST") || tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE") != "100" {
		t.Errorf("VOD playlist malformed:\n%s", out)
	}
}

// A multi-period DASH source that continues its numbering keeps its numbers.
func TestMediaKeepsNumberingThatContinuesAcrossPeriods(t *testing.T) {
	out := string(Media(timeline(
		testPeriod{id: "a", segs: segRange(0, 4)},
		testPeriod{id: "b", segs: segRange(5, 9)},
	), mediaOpts()))
	if got := tagValue(t, out, "#EXT-X-MEDIA-SEQUENCE"); got != "0" {
		t.Errorf("media sequence = %s, want 0", got)
	}
	if got := playlistSegments(out); len(got) != 10 || got[9] != "/segments/b/v0/9.ts" {
		t.Errorf("segments = %v", got)
	}
}

// discontinuitySequences maps each segment URI path a playlist lists to its
// discontinuity sequence number: EXT-X-DISCONTINUITY-SEQUENCE plus the
// EXT-X-DISCONTINUITY tags before it.
func discontinuitySequences(out string) map[string]int {
	m := map[string]int{}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			n, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"))
		case line == "#EXT-X-DISCONTINUITY":
			n++
		case strings.Contains(line, "/segments/"):
			m[line[strings.Index(line, "/segments/"):]] = n
		}
	}
	return m
}

// As a live window slides, every segment it still shows keeps its
// discontinuity sequence number (RFC 8216 §6.2.2): a discontinuity whose tag
// leaves the playlist — or is dropped because its segment now opens the
// playlist — is carried by EXT-X-DISCONTINUITY-SEQUENCE instead. That holds
// for a period boundary and for an origin discontinuity inside a period,
// which the representation's lifetime record still knows once its segment has
// gone. A discontinuity on a period's first segment is the boundary itself.
func TestDiscontinuitySequenceStableWhileTheWindowSlides(t *testing.T) {
	// period holds segments present..6 of a period that has published 1..6,
	// with origin discontinuities at discs.
	period := func(id string, present uint64, discs ...uint64) testPeriod {
		segs := segRange(1, 6)
		for i := range segs {
			for _, d := range discs {
				if segs[i].SegNo == d {
					segs[i].Discontinuity = true
				}
			}
		}
		return testPeriod{id: id, segs: segs, present: present}
	}
	gone := func(p testPeriod) testPeriod { p.present = 7; return p }
	for _, tc := range []struct {
		name          string
		before, after []testPeriod
	}{
		{"origin discontinuity leaves the window",
			[]testPeriod{period("0", 1, 4)}, []testPeriod{period("0", 5, 4)}},
		{"origin discontinuity opens the window",
			[]testPeriod{period("0", 1, 4)}, []testPeriod{period("0", 4, 4)}},
		{"several origin discontinuities leave the window",
			[]testPeriod{period("0", 1, 2, 3, 5)}, []testPeriod{period("0", 6, 2, 3, 5)}},
		{"period boundary leaves the window",
			[]testPeriod{period("0", 4), period("1", 1)}, []testPeriod{gone(period("0", 4)), period("1", 2)}},
		{"earlier period's origin discontinuity leaves the window",
			[]testPeriod{period("0", 3, 4), period("1", 1)}, []testPeriod{gone(period("0", 3, 4)), period("1", 2)}},
		{"discontinuity on a period's first segment is its boundary",
			[]testPeriod{period("0", 4), period("1", 1, 1, 3)}, []testPeriod{gone(period("0", 4)), period("1", 4, 1, 3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := string(Media(timeline(tc.before...), mediaOpts())), string(Media(timeline(tc.after...), mediaOpts()))
			was, is := discontinuitySequences(before), discontinuitySequences(after)
			if len(is) == 0 {
				t.Fatalf("the slid playlist lists no segments:\n%s", after)
			}
			for uri, n := range is {
				if m, ok := was[uri]; !ok {
					t.Errorf("%s is not in the playlist before the slide:\n%s", uri, before)
				} else if m != n {
					t.Errorf("%s: discontinuity sequence %d before the slide, %d after\nbefore:\n%s\nafter:\n%s", uri, m, n, before, after)
				}
			}
		})
	}
}
