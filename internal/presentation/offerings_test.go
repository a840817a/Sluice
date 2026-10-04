package presentation

import "testing"

func TestOfferings(t *testing.T) {
	const mpd, m3u8 = "/v1/channels/c1/manifest.mpd", "/v1/channels/c1/master.m3u8"
	fmp4 := Tracks{ServesHLS: true, ServesDASH: true, Ready: true}
	ts := Tracks{ServesHLS: true, Ready: true}
	for _, tc := range []struct {
		name   string
		ch     Channel
		native string
		alt    string
	}{
		{"fMP4 HLS source", Channel{ConfigIsHLS: true, IsHLSSource: true, Tracks: fmp4}, m3u8, mpd},
		{"MPEG-TS HLS source", Channel{ConfigIsHLS: true, IsHLSSource: true, Tracks: ts}, m3u8, ""},
		// The configuration was switched to HLS but the DASH runtime still
		// runs: it serves DASH, yet no synthesized MPD backs an HLS claim.
		{"HLS configured, DASH runtime", Channel{ConfigIsHLS: true, Tracks: fmp4}, m3u8, ""},
		{"clear DASH source", Channel{Tracks: fmp4}, mpd, m3u8},
		{"DASH source before its tracks are known", Channel{Tracks: Tracks{ServesHLS: true, ServesDASH: true}}, mpd, ""},
		{"encrypted DASH source", Channel{Tracks: Tracks{ServesDASH: true}}, mpd, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.ch.ID = "c1"
			got := testGenerator().Offerings(tc.ch)
			if got.Native != tc.native || got.Alt != tc.alt {
				t.Errorf("Offerings = %+v, want native %q alt %q", got, tc.native, tc.alt)
			}
		})
	}
}
