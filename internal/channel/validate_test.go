package channel

import "testing"

// Config.Validate is shared by the admin API's create and update handlers,
// and its error strings reach clients verbatim. httpapi's own tests only ever
// print response bodies inside failure messages, so without this the text —
// including the "mpd_url: " prefix clients key on — is free to drift.
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string // "" means the config must be accepted
	}{
		{"valid dash", Config{MPDURL: "http://example.com/live.mpd"}, ""},
		{"valid hls passthrough", Config{
			MPDURL:     "https://example.com/index.m3u8",
			SourceType: SourceHLS,
			HLSKeyMode: KeyModePassthrough,
		}, ""},
		{"empty source type and key mode default", Config{
			MPDURL: "http://example.com/live.mpd",
		}, ""},

		{"bad scheme keeps the mpd_url prefix", Config{MPDURL: "file:///etc/passwd"},
			"mpd_url: mpd_url must use http or https scheme"},
		{"empty url keeps the mpd_url prefix", Config{MPDURL: ""},
			"mpd_url: mpd_url must use http or https scheme"},
		{"bad source type", Config{
			MPDURL:     "http://example.com/live.mpd",
			SourceType: "rtmp",
		}, `source_type must be "dash" or "hls"`},
		{"bad key mode", Config{
			MPDURL:     "http://example.com/live.mpd",
			HLSKeyMode: "sample-aes",
		}, `hls_key_mode must be "passthrough" or "decrypt"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateSourceURL(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"http://example.com/live.mpd", false},
		{"https://cdn.example.com/stream.mpd", false},
		{"file:///etc/passwd", true},
		{"ftp://example.com/stream.mpd", true},
		{"//no-scheme.com/", true},
		{"not a url", true},
		{"", true},
	}
	for _, tc := range cases {
		err := validateSourceURL(tc.url)
		if tc.wantErr && err == nil {
			t.Errorf("validateSourceURL(%q): expected error, got nil", tc.url)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateSourceURL(%q): unexpected error: %v", tc.url, err)
		}
	}
}
