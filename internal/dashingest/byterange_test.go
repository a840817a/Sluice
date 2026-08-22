package dashingest

import "testing"

// parseByteRange feeds SegmentBase/SegmentList byte ranges straight from the
// upstream MPD, so a malformed range must be rejected rather than silently
// turned into a range that reads the wrong bytes.
func TestParseByteRange(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		wantStart        uint64
		wantEnd          uint64
		wantOK           bool
		wantOKExplainWhy string
	}{
		{name: "normal", in: "0-99", wantStart: 0, wantEnd: 99, wantOK: true},
		{name: "offset range", in: "1200-3455", wantStart: 1200, wantEnd: 3455, wantOK: true},

		{name: "missing start", in: "-100", wantOK: false,
			wantOKExplainWhy: "an empty start must not parse as 0"},
		{name: "missing end", in: "100-", wantOK: false,
			wantOKExplainWhy: "an empty end must not parse as 0"},
		{name: "both empty", in: "-", wantOK: false},

		{name: "start overflows uint64", in: "99999999999999999999999-1", wantOK: false,
			wantOKExplainWhy: "overflow must be rejected, not wrapped"},
		{name: "end overflows uint64", in: "1-99999999999999999999999", wantOK: false,
			wantOKExplainWhy: "overflow must be rejected, not wrapped"},

		{name: "no separator", in: "1234", wantOK: false},
		{name: "non numeric", in: "abc-1", wantOK: false},
		{name: "negative end", in: "1--5", wantOK: false},
		{name: "empty", in: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := parseByteRange(tc.in)
			if ok != tc.wantOK {
				why := tc.wantOKExplainWhy
				if why == "" {
					why = "unexpected ok value"
				}
				t.Fatalf("parseByteRange(%q) ok = %v, want %v (%s)", tc.in, ok, tc.wantOK, why)
			}
			if !tc.wantOK {
				return
			}
			if start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("parseByteRange(%q) = (%d, %d), want (%d, %d)",
					tc.in, start, end, tc.wantStart, tc.wantEnd)
			}
		})
	}
}
