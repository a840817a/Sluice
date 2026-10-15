package gwurl

import (
	"strings"
	"testing"
)

// A representation ID that already worked as a URL path segment keeps its
// URL byte for byte: players and CDNs hold them.
func TestRepresentationURLsOfWorkingIDsAreUnchanged(t *testing.T) {
	for _, id := range []string{"v0", "mp4a.40.2", "audio=128000", "video_eng=1500000", "a b", "é", "1"} {
		want := "http://gw/v1/channels/c/init/0/" + id + ".mp4"
		if got := InitURL("http://gw", "c", "0", id); got != want {
			t.Errorf("InitURL for %q = %q, want %q", id, got, want)
		}
		want = "http://gw/v1/channels/c/segments/0/" + id + "/7.m4s"
		if got := SegmentURL("http://gw", "c", "0", id, 7, ".m4s"); got != want {
			t.Errorf("SegmentURL for %q = %q, want %q", id, got, want)
		}
		want = "http://gw/v1/channels/c/media/" + id + ".m3u8"
		if got := MediaPlaylistURL("http://gw", "c", id); got != want {
			t.Errorf("MediaPlaylistURL for %q = %q, want %q", id, got, want)
		}
		want = "v1/channels/c/segments/0/" + id + "/$Number%09d$.m4s"
		if got := SegmentTemplatePath("c", "0", id); got != want {
			t.Errorf("SegmentTemplatePath for %q = %q, want %q", id, got, want)
		}
		want = "v1/channels/c/init/0/" + id + ".mp4"
		if got := InitTemplatePath("c", "0", id); got != want {
			t.Errorf("InitTemplatePath for %q = %q, want %q", id, got, want)
		}
	}
}

// Any representation ID survives the trip through one URL path segment, and
// each ID has exactly one segment.
func TestRepresentationSegmentRoundTrips(t *testing.T) {
	for _, id := range []string{"", ".", "..", "~", "~YQ", "v0", "video/avc1", "audio/und/mp4a.40.2",
		`a\b`, "a?b", "a#b", "a%2Fb", "../../x", "audio=128000", "mp4a.40.2"} {
		seg := repSegment(id)
		if strings.ContainsAny(seg, `/\?#%`) || seg == "" || seg == "." || seg == ".." {
			t.Errorf("segment for %q is %q, which is not one path segment", id, seg)
		}
		if got, ok := ParseRepSegment(seg); !ok || got != id {
			t.Errorf("ParseRepSegment(%q) = %q, %v; want %q, true", seg, got, ok, id)
		}
	}
	// "~djA" is base64url for "v0", which is written as is: accepting it
	// would give v0 a second URL.
	for _, seg := range []string{"~djA", "~!!", "", "a/b", "a%2Fb"} {
		if id, ok := ParseRepSegment(seg); ok {
			t.Errorf("ParseRepSegment(%q) = %q, true; want false", seg, id)
		}
	}
}
