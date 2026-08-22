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
