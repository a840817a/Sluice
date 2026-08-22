package httpapi

import "testing"

// hlsout and manifest assert their emitted URLs byte-exactly in their own
// packages. httpapi's two builders had no exact-string assertion anywhere, even
// though both feed URLs that players fetch: licenseProxyURL goes into the DASH
// manifest's ContentProtection, keyProxyURL into the HLS playlist's EXT-X-KEY.
// A trailing-slash or path regression in either produces a manifest that parses
// fine and plays nothing.

func TestLicenseProxyURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base      string
		channelID string
		drmType   string
		enabled   bool
		want      string
	}{
		{"playready", "http://gw.test", "ch1", "playready", true,
			"http://gw.test/v1/channels/ch1/license/playready"},
		{"widevine", "http://gw.test", "ch1", "widevine", true,
			"http://gw.test/v1/channels/ch1/license/widevine"},
		// A base URL configured with a trailing slash must not double the
		// separator; config.yaml examples are written both ways.
		{"trailing slash", "http://gw.test/", "ch1", "playready", true,
			"http://gw.test/v1/channels/ch1/license/playready"},
		{"multiple trailing slashes", "http://gw.test///", "ch1", "playready", true,
			"http://gw.test/v1/channels/ch1/license/playready"},
		{"base with path", "https://cdn.example.com/gw", "ch1", "widevine", true,
			"https://cdn.example.com/gw/v1/channels/ch1/license/widevine"},
		// Disabled DRM must yield an empty string, not a dead URL: the manifest
		// generator uses "" to mean "omit this ContentProtection entirely".
		{"disabled", "http://gw.test", "ch1", "playready", false, ""},
		{"disabled with trailing slash", "http://gw.test/", "ch1", "widevine", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := licenseProxyURL(tc.base, tc.channelID, tc.drmType, tc.enabled)
			if got != tc.want {
				t.Errorf("licenseProxyURL(%q, %q, %q, %v) = %q, want %q",
					tc.base, tc.channelID, tc.drmType, tc.enabled, got, tc.want)
			}
		})
	}
}

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
