package pathseg

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

var ids = []string{"", "_", "~", "0", "10", "p1", "P-1.a_b", "a/b", `a\b`, "../../x", "p 1", "x$Number$", "%", "%41", ".", "..", "é", "~YQ"}

// An ID must survive the trip through one URL path segment, whatever the
// upstream called it, and stay out of DASH template syntax.
//
// A router may hand the handler either the escaped or the decoded form of the
// segment, depending on how the request path was escaped, so both must decode
// to the same ID.
func TestEncodeRoundTripsThroughAURL(t *testing.T) {
	for _, id := range ids {
		seg := Encode(id)
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/$% ") {
			t.Errorf("Encode(%q) = %q, not a safe path segment", id, seg)
		}
		u, err := url.Parse("http://gw/v1/channels/c/segments/" + seg + "/v0/1.m4s")
		if err != nil {
			t.Fatalf("Encode(%q) = %q does not parse in a URL: %v", id, seg, err)
		}
		for _, form := range []string{strings.Split(u.EscapedPath(), "/")[5], strings.Split(u.Path, "/")[5]} {
			if got, ok := Decode(form); !ok || got != id {
				t.Errorf("Decode(%q) = %q, %v; want %q", form, got, ok, id)
			}
		}
	}
	// "~YQ" is "a" encoded, which Encode writes as "a": accepting both would
	// give one ID two segments.
	for _, bad := range []string{"", "..", "a%2Fb", "~!!", "~YQ"} {
		if _, ok := Decode(bad); ok {
			t.Errorf("Decode(%q) accepted a segment Encode never writes", bad)
		}
	}
}

// As a directory name an encoded ID must stay exactly one level below its
// parent: a join neither drops it nor climbs out.
func TestEncodeStaysOneDirectoryLevel(t *testing.T) {
	const parent = "/data/ch/periods"
	for _, id := range ids {
		p := filepath.Join(parent, Encode(id))
		if filepath.Dir(p) != parent {
			t.Errorf("Join(%q, Encode(%q)) = %q, not one level below it", parent, id, p)
			continue
		}
		if got, ok := Decode(filepath.Base(p)); !ok || got != id {
			t.Errorf("Decode(Base(%q)) = %q, %v; want %q", p, got, ok, id)
		}
	}
}
