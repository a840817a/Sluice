package mpd

import (
	"testing"
	"time"
)

// LastSegmentNumber feeds the static-MPD discovery loop's upper bound. Returning
// a wrapped-around number there produces an effectively infinite segment range,
// so "no segments" must come back as 0 rather than underflowing.
func TestLastSegmentNumberNoUnderflow(t *testing.T) {
	tests := []struct {
		name        string
		tmpl        ParsedSegmentTemplate
		periodDur   time.Duration
		want        uint64
		wantComment string
	}{
		{
			name:      "typical 4s segments over 40s",
			tmpl:      ParsedSegmentTemplate{Duration: 4 * 90000, Timescale: 90000, StartNumber: 1},
			periodDur: 40 * time.Second,
			want:      10,
		},
		{
			name:      "startNumber 0 is respected",
			tmpl:      ParsedSegmentTemplate{Duration: 4 * 90000, Timescale: 90000, StartNumber: 0},
			periodDur: 40 * time.Second,
			want:      9,
		},
		{
			name:        "period shorter than one tick with startNumber 0",
			tmpl:        ParsedSegmentTemplate{Duration: 1, Timescale: 1000, StartNumber: 0},
			periodDur:   time.Microsecond,
			want:        0,
			wantComment: "must not underflow to MaxUint64",
		},
		{
			name:        "period shorter than one tick with startNumber 1",
			tmpl:        ParsedSegmentTemplate{Duration: 1, Timescale: 1000, StartNumber: 1},
			periodDur:   time.Microsecond,
			want:        0,
			wantComment: "no segments means 0, not StartNumber-1",
		},
		{
			name:      "zero duration",
			tmpl:      ParsedSegmentTemplate{Duration: 0, Timescale: 90000, StartNumber: 1},
			periodDur: 40 * time.Second,
			want:      0,
		},
		{
			name:      "negative period duration",
			tmpl:      ParsedSegmentTemplate{Duration: 90000, Timescale: 90000, StartNumber: 1},
			periodDur: -time.Second,
			want:      0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tmpl.LastSegmentNumber(tc.periodDur)
			if got != tc.want {
				t.Errorf("LastSegmentNumber(%v) = %d, want %d %s",
					tc.periodDur, got, tc.want, tc.wantComment)
			}
		})
	}
}

// A representation may override only URLs, or explicitly reset a parent PTO.
func TestTemplatePresentationTimeOffsetInheritance(t *testing.T) {
	for _, tc := range []struct {
		name, attrs string
		want        uint64
	}{{"inherited", "", 100}, {"explicit zero", `presentationTimeOffset="0"`, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse([]byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><SegmentTemplate timescale="10" presentationTimeOffset="100"/><Representation id="v"><SegmentTemplate media="$Number$.m4s" ` + tc.attrs + `/></Representation></AdaptationSet></Period></MPD>`))
			if err != nil {
				t.Fatal(err)
			}
			as := p.Periods[0].AdaptationSets[0]
			if got := as.Representations[0].EffectiveTemplate(as).PresentationTimeOffset; got != tc.want {
				t.Fatalf("offset=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestListAndBasePresentationTimeOffsetInheritance(t *testing.T) {
	for _, kind := range []string{"SegmentList", "SegmentBase"} {
		for _, tc := range []struct {
			name, attrs string
			want        uint64
		}{{"inherited", "", 200}, {"explicit zero", `presentationTimeOffset="0"`, 0}} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				p, err := Parse([]byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><` + kind + ` timescale="10" presentationTimeOffset="200"/><Representation id="a"><` + kind + ` ` + tc.attrs + `/></Representation></AdaptationSet></Period></MPD>`))
				if err != nil {
					t.Fatal(err)
				}
				as := p.Periods[0].AdaptationSets[0]
				rep := as.Representations[0]
				var offset, scale uint64
				if kind == "SegmentList" {
					v := rep.EffectiveSegmentList(as)
					offset, scale = v.PresentationTimeOffset, v.Timescale
				} else {
					v := rep.EffectiveSegmentBase(as)
					offset, scale = v.PresentationTimeOffset, uint64(v.Timescale)
				}
				if offset != tc.want || scale != 10 {
					t.Fatalf("offset/scale=%d/%d, want %d/10", offset, scale, tc.want)
				}
			})
		}
	}
}
