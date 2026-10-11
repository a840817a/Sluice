package hlsout

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

// The segment section is written by one set of rules for served and local
// playlists; this pins each rule on its own.
func TestSegmentLines(t *testing.T) {
	type spec struct {
		no        uint64
		period    int
		disc      bool
		init, key string
	}
	// Each entry is one second long, written "no@period".
	lines := segmentLines{
		extinf:  "#EXTINF:%.1f,\n",
		uri:     func(e playlistEntry) (string, error) { return fmt.Sprintf("%d@%d", e.seg.SegNo, e.periodIdx), nil },
		initURI: func(e playlistEntry) (string, error) { return e.seg.InitPath, nil },
		keyLine: func(s index.SegmentState) (string, error) {
			if s.KeyURI == "" {
				return "", nil
			}
			return "#EXT-X-KEY:" + s.KeyURI, nil
		},
	}
	render := func(specs ...spec) string {
		var entries []playlistEntry
		for _, s := range specs {
			entries = append(entries, playlistEntry{periodIdx: s.period, seg: index.SegmentState{
				SegNo: s.no, Discontinuity: s.disc, InitPath: s.init, KeyURI: s.key,
				StartPTS: 0, EndPTS: 1, Timescale: 1,
			}})
		}
		var b strings.Builder
		if err := lines.write(&b, entries); err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(strings.TrimSpace(strings.ReplaceAll(b.String(), "#EXTINF:1.0,\n", "")), "\n", " | ")
	}
	for _, tc := range []struct {
		name  string
		specs []spec
		want  string
	}{
		{"a discontinuity on the first entry is not written",
			[]spec{{no: 1, disc: true}, {no: 2}}, "1@0 | 2@0"},
		{"an origin discontinuity, a skip and a period boundary each break",
			[]spec{{no: 1}, {no: 2, disc: true}, {no: 4}, {no: 5, period: 1}},
			"1@0 | #EXT-X-DISCONTINUITY | 2@0 | #EXT-X-DISCONTINUITY | 4@0 | #EXT-X-DISCONTINUITY | 5@1"},
		{"an init is mapped first, at a boundary and when it changes",
			[]spec{{no: 1, init: "i0"}, {no: 2, init: "i0"}, {no: 3, init: "i1"}, {no: 4, period: 1, init: "i1"}},
			`#EXT-X-MAP:URI="i0" | 1@0 | 2@0 | #EXT-X-MAP:URI="i1" | 3@0 | #EXT-X-DISCONTINUITY | #EXT-X-MAP:URI="i1" | 4@1`},
		{"a key is written when it changes and ended when encryption stops",
			[]spec{{no: 1, key: "k1"}, {no: 2, key: "k1"}, {no: 3, key: "k2"}, {no: 4}},
			"#EXT-X-KEY:k1 | 1@0 | 2@0 | #EXT-X-KEY:k2 | 3@0 | #EXT-X-KEY:METHOD=NONE | 4@0"},
		{"a key in force is ended before a clear init and written again after it",
			[]spec{{no: 1, init: "i0", key: "k1"}, {no: 2, period: 1, init: "i1", key: "k1"}},
			`#EXT-X-MAP:URI="i0" | #EXT-X-KEY:k1 | 1@0 | #EXT-X-DISCONTINUITY | #EXT-X-KEY:METHOD=NONE | #EXT-X-MAP:URI="i1" | #EXT-X-KEY:k1 | 2@1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.specs...); got != tc.want {
				t.Errorf("got\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}

	t.Run("an address that cannot be written fails the playlist", func(t *testing.T) {
		bad := lines
		bad.uri = func(playlistEntry) (string, error) { return "", errors.New("outside") }
		var b strings.Builder
		if err := bad.write(&b, []playlistEntry{{seg: index.SegmentState{SegNo: 1}}}); err == nil {
			t.Error("write succeeded")
		}
	})
}
