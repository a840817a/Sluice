package httpapi

import "testing"

// hlsout and manifest assert their emitted URLs byte-exactly in their own
// packages. keyProxyURL goes into the HLS playlist's EXT-X-KEY: a
// trailing-slash or path regression produces a playlist that parses fine and
// plays nothing.

func TestKeyProxyURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base      string
		channelID string
		want      string
	}{
		{"plain", "http://gw.test", "ch1", "http://gw.test/v1/channels/ch1/key"},
		{"trailing slash", "http://gw.test/", "ch1", "http://gw.test/v1/channels/ch1/key"},
		{"multiple trailing slashes", "http://gw.test///", "ch1", "http://gw.test/v1/channels/ch1/key"},
		{"base with path", "https://cdn.example.com/gw", "ch1", "https://cdn.example.com/gw/v1/channels/ch1/key"},
		{"channel id with dashes", "http://gw.test", "my-ch_1", "http://gw.test/v1/channels/my-ch_1/key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keyProxyURL(tc.base, tc.channelID)
			if got != tc.want {
				t.Errorf("keyProxyURL(%q, %q) = %q, want %q", tc.base, tc.channelID, got, tc.want)
			}
		})
	}
}

// The key proxy URL is a prefix the playlist appends "/{keyID}" to, so it must
// not already end in a slash. This is the property that actually matters, and
// it is one substitution away from being silently wrong.
func TestKeyProxyURLHasNoTrailingSlash(t *testing.T) {
	for _, base := range []string{"http://gw.test", "http://gw.test/", "http://gw.test//"} {
		got := keyProxyURL(base, "ch1")
		if got[len(got)-1] == '/' {
			t.Errorf("keyProxyURL(%q, ch1) = %q, must not end in a slash", base, got)
		}
	}
}
