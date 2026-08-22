package channel

import (
	"slices"
	"testing"
)

// base is a fully populated config so each case can change exactly one field.
func base() Config {
	return Config{
		Title:               "t",
		SourceType:          SourceDASH,
		HLSKeyMode:          KeyModePassthrough,
		MPDURL:              "http://example.com/a.mpd",
		PlayReadyLicenseURL: "http://drm.example.com/pr",
		WidevineLicenseURL:  "http://drm.example.com/wv",
		EnablePlayReady:     BoolPtr(true),
		EnableWidevine:      BoolPtr(true),
		FetchHeaders:        []Header{{Name: "X-Token", Value: "a"}},
		LicenseHeaders:      []Header{{Name: "X-Lic", Value: "a"}},
		ForwardClientIP:     true,
		Enabled:             true,
	}
}

func TestRestartRequired(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		live   []string // blockers in live mode
		vod    []string // blockers in VOD mode
	}{
		// Hot fields: read per request by the HTTP handlers, or (FetchHeaders)
		// read through the provider ApplyConfig swaps.
		{"title", func(c *Config) { c.Title = "new" }, nil, nil},
		{"playready url", func(c *Config) { c.PlayReadyLicenseURL = "http://x" }, nil, nil},
		{"widevine url", func(c *Config) { c.WidevineLicenseURL = "http://x" }, nil, nil},
		{"playready off", func(c *Config) { c.EnablePlayReady = BoolPtr(false) }, nil, nil},
		{"widevine off", func(c *Config) { c.EnableWidevine = BoolPtr(false) }, nil, nil},
		{"license headers", func(c *Config) { c.LicenseHeaders = []Header{{Name: "X-Lic", Value: "b"}} }, nil, nil},
		{"forward client ip", func(c *Config) { c.ForwardClientIP = false }, nil, nil},
		{"fetch headers", func(c *Config) { c.FetchHeaders = []Header{{Name: "X-Token", Value: "b"}} }, nil, nil},

		// Frozen into long-lived state at Start.
		{"source type", func(c *Config) { c.SourceType = SourceHLS }, []string{"source_type"}, []string{"source_type"}},
		{"key mode", func(c *Config) { c.HLSKeyMode = KeyModeDecrypt }, []string{"hls_key_mode"}, []string{"hls_key_mode"}},
		{"is vod", func(c *Config) { c.IsVOD = true }, []string{"is_vod"}, []string{"is_vod"}},

		// Ingest-only: inert once the channel is serving from disk in VOD mode.
		{"mpd url", func(c *Config) { c.MPDURL = "http://example.com/b.mpd" }, []string{"mpd_url"}, nil},
		{"keep all", func(c *Config) { c.KeepAllSegments = true }, []string{"keep_all_segments"}, nil},
		{"vod transition", func(c *Config) { c.EnableVODTransition = true }, []string{"enable_vod_transition"}, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := base()
			next := base()
			tc.mutate(&next)

			if got := RestartRequired(old, next, ModeLive); !slices.Equal(got, tc.live) {
				t.Errorf("live: got %v, want %v", got, tc.live)
			}
			if got := RestartRequired(old, next, ModeVOD); !slices.Equal(got, tc.vod) {
				t.Errorf("vod: got %v, want %v", got, tc.vod)
			}
			// Transitioning and static-ingesting still have ingest goroutines
			// running, so they must be as conservative as live.
			for _, m := range []ChannelMode{ModeTransitioning, ModeStaticIngesting} {
				if got := RestartRequired(old, next, m); !slices.Equal(got, tc.live) {
					t.Errorf("mode %d: got %v, want %v (same as live)", m, got, tc.live)
				}
			}
		})
	}
}

func TestRestartRequiredNoChange(t *testing.T) {
	for _, m := range []ChannelMode{ModeLive, ModeVOD, ModeTransitioning, ModeStaticIngesting} {
		if got := RestartRequired(base(), base(), m); len(got) != 0 {
			t.Errorf("mode %d: identical configs reported %v", m, got)
		}
	}
}

// A nil *bool means "enabled", so filling it in explicitly is not a change and
// must not cost the operator a restart or a needless config swap.
func TestRestartRequiredTreatsNilBoolAsEnabled(t *testing.T) {
	old := base()
	old.EnablePlayReady = nil
	old.EnableWidevine = nil
	next := base() // BoolPtr(true) for both

	if got := RestartRequired(old, next, ModeLive); len(got) != 0 {
		t.Errorf("nil→true reported %v, want no restart", got)
	}
	if !old.PlayReadyEnabled() || !next.PlayReadyEnabled() {
		t.Error("nil and BoolPtr(true) should both read as enabled")
	}
}

// An empty SourceType means "dash" and an empty HLSKeyMode means "passthrough".
// The admin UI normalises both when it loads a channel and submits the explicit
// value, so treating "" as different from its default would restart every
// channel created before those fields existed on its first save — the exact
// interruption this whole feature is meant to avoid.
func TestRestartRequiredTreatsEmptyAsDefault(t *testing.T) {
	stored := base()
	stored.SourceType = ""
	stored.HLSKeyMode = ""

	submitted := base()
	submitted.SourceType = SourceDASH
	submitted.HLSKeyMode = KeyModePassthrough

	if got := RestartRequired(stored, submitted, ModeLive); len(got) != 0 {
		t.Errorf("empty→explicit default reported %v, want no restart", got)
	}
	// The reverse direction matters too: the store may hold the explicit value
	// while an API client omits the field entirely.
	if got := RestartRequired(submitted, stored, ModeLive); len(got) != 0 {
		t.Errorf("explicit default→empty reported %v, want no restart", got)
	}
	// A genuine change must still be caught.
	real := base()
	real.SourceType = SourceHLS
	if got := RestartRequired(stored, real, ModeLive); len(got) != 1 || got[0] != "source_type" {
		t.Errorf("dash→hls reported %v, want [source_type]", got)
	}
}

// The VOD set must stay a subset of the live set: Mode can change between the
// restart decision and the swap, and only monotonic relaxation makes that safe.
func TestVODBlockersAreSubsetOfLive(t *testing.T) {
	mutations := []func(*Config){
		func(c *Config) { c.SourceType = SourceHLS },
		func(c *Config) { c.HLSKeyMode = KeyModeDecrypt },
		func(c *Config) { c.MPDURL = "http://other" },
		func(c *Config) { c.KeepAllSegments = true },
		func(c *Config) { c.EnableVODTransition = true },
		func(c *Config) { c.IsVOD = true },
		func(c *Config) { c.Title = "x" },
	}
	for i, mutate := range mutations {
		old, next := base(), base()
		mutate(&next)
		live := RestartRequired(old, next, ModeLive)
		for _, f := range RestartRequired(old, next, ModeVOD) {
			if !slices.Contains(live, f) {
				t.Errorf("mutation %d: %q restarts in VOD but not in live", i, f)
			}
		}
	}
}
