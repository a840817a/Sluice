package presentation

import "testing"

// manifest asserts its emitted URLs byte-exactly in its own package, but the
// license URL it is handed is built here and goes into the DASH manifest's
// ContentProtection. A trailing-slash or path regression produces a manifest
// that parses fine and plays nothing.

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
			got := licenseURL(tc.base, tc.channelID, tc.drmType, tc.enabled)
			if got != tc.want {
				t.Errorf("licenseURL(%q, %q, %q, %v) = %q, want %q",
					tc.base, tc.channelID, tc.drmType, tc.enabled, got, tc.want)
			}
		})
	}
}
