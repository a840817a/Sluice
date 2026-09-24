package gwurl

import (
	"net/url"
	"strings"
	"testing"
)

// A period ID must survive the trip through one URL path segment, whatever the
// upstream MPD called it, and stay out of DASH template syntax.
//
// A router may hand the handler either the escaped or the decoded form of the
// segment, depending on how the request path was escaped, so both must decode
// to the same ID.
func TestPeriodSegmentRoundTrips(t *testing.T) {
	for _, id := range []string{"", "_", "~", "0", "10", "p1", "P-1.a_b", "a/b", "p 1", "x$Number$", "%", "%41", ".", "..", "é", "~YQ"} {
		seg := PeriodSegment(id)
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/$% ") {
			t.Errorf("PeriodSegment(%q) = %q, not a safe path segment", id, seg)
		}
		u, err := url.Parse("http://gw/v1/channels/c/segments/" + seg + "/v0/1.m4s")
		if err != nil {
			t.Fatalf("PeriodSegment(%q) = %q does not parse in a URL: %v", id, seg, err)
		}
		for _, form := range []string{strings.Split(u.EscapedPath(), "/")[5], strings.Split(u.Path, "/")[5]} {
			if got, ok := ParsePeriodSegment(form); !ok || got != id {
				t.Errorf("ParsePeriodSegment(%q) = %q, %v; want %q", form, got, ok, id)
			}
		}
	}
	for _, bad := range []string{"", "..", "a%2Fb", "~!!"} {
		if _, ok := ParsePeriodSegment(bad); ok {
			t.Errorf("ParsePeriodSegment(%q) accepted a segment PeriodSegment never writes", bad)
		}
	}
}
