package hlsingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// hls-source.json is the only record of the upstream master playlist's
// attributes: a channel restored in VOD mode never runs discovery again, so a
// field lost in this round trip means representations the gateway can name but
// not serve. Until now the format was only covered indirectly, through the
// httpapi restart end-to-end test.

func stateWatcher(t *testing.T, path string) *Watcher {
	t.Helper()
	return NewWatcher(config.Config{}, "ch1", "http://origin.example.com/master.m3u8",
		nil, false, path, index.NewChannelIndex(), queue.NewBroker(16))
}

func TestSourceStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hls-source.json")

	want := []variant{
		{
			Variant: hlsdesc.Variant{
				RepID:            "v0",
				Bandwidth:        2_000_000,
				AverageBandwidth: 1_800_000,
				Codecs:           "avc1.4d401f",
				Width:            1280,
				Height:           720,
				FrameRate:        29.97,
				AudioGroup:       "aac",
				SubtitleGroup:    "subs",
			},
			url:       "http://origin.example.com/v0.m3u8",
			asID:      hlsASID,
			mediaType: hlsMediaType,
		},
		{
			Variant:   hlsdesc.Variant{RepID: "a0"},
			url:       "http://origin.example.com/a0.m3u8",
			asID:      AudioASID,
			mediaType: "audio",
			rendition: true,
			kind:      renditionAudio,
			groupID:   "aac",
			name:      "English",
			language:  "en",
			isDefault: true,
			channels:  "2",
		},
		{
			Variant:   hlsdesc.Variant{RepID: "s0"},
			url:       "http://origin.example.com/s0.m3u8",
			asID:      SubtitleASID,
			mediaType: "text",
			rendition: true,
			kind:      renditionSubtitle,
			groupID:   "subs",
			name:      "English CC",
			language:  "en",
			forced:    true,
		},
	}

	w := stateWatcher(t, path)
	w.variants = want
	w.independentSegments = true
	w.fmp4 = true
	w.keyURIs = map[string]string{"abc123": "https://keys.example.com/k1"}
	w.saveState()

	got := stateWatcher(t, path) // NewWatcher calls loadState

	if len(got.variants) != len(want) {
		t.Fatalf("restored %d variants, want %d", len(got.variants), len(want))
	}
	for i := range want {
		if got.variants[i] != want[i] {
			t.Errorf("variant %d round-tripped as\n  %+v\nwant\n  %+v", i, got.variants[i], want[i])
		}
	}
	if !got.independentSegments {
		t.Error("independentSegments lost")
	}
	if !got.fmp4 {
		t.Error("fmp4 lost")
	}
	if uri, ok := got.KeyURIForID("abc123"); !ok || uri != "https://keys.example.com/k1" {
		t.Errorf("key URI round-tripped as %q (ok=%v)", uri, ok)
	}
}

// The embedded descriptor must keep its snake_case keys, not the PascalCase Go
// field names an untagged embedded struct would produce.
func TestSourceStateJSONKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hls-source.json")

	w := stateWatcher(t, path)
	w.variants = []variant{{
		Variant: hlsdesc.Variant{RepID: "v0", Bandwidth: 1234, Codecs: "avc1", Width: 640, Height: 360},
		url:     "http://origin.example.com/v0.m3u8",
		asID:    hlsASID,
	}}
	w.saveState()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var raw struct {
		Version  int `json:"version"`
		Variants []map[string]any
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.Version != sourceStateVersion {
		t.Errorf("version = %d, want %d", raw.Version, sourceStateVersion)
	}
	if len(raw.Variants) != 1 {
		t.Fatalf("got %d variants", len(raw.Variants))
	}
	for _, key := range []string{"rep_id", "bandwidth", "codecs", "width", "height", "url"} {
		if _, ok := raw.Variants[0][key]; !ok {
			t.Errorf("key %q missing from persisted variant: %v", key, raw.Variants[0])
		}
	}
	for _, bad := range []string{"RepID", "Bandwidth", "Codecs"} {
		if _, ok := raw.Variants[0][bad]; ok {
			t.Errorf("key %q leaked Go field naming into the file", bad)
		}
	}
}

// A state file from a different layout must be discarded, not half-loaded.
// encoding/json ignores unknown keys, so without the version guard an old file
// would restore variants with empty RepIDs and the channel would serve nothing.
func TestSourceStateWrongVersionIsDiscarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no version field", `{"variants":[{"rep_id":"v0","url":"http://o/v0.m3u8"}],"fmp4":true}`},
		{"older version", `{"version":1,"variants":[{"rep_id":"v0","url":"http://o/v0.m3u8"}]}`},
		{"newer version", `{"version":99,"variants":[{"rep_id":"v0","url":"http://o/v0.m3u8"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hls-source.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			w := stateWatcher(t, path)
			if len(w.variants) != 0 {
				t.Errorf("restored %d variants from an unsupported version, want 0", len(w.variants))
			}
			if w.fmp4 {
				t.Error("fmp4 restored from an unsupported version")
			}
		})
	}
}

func TestSourceStateMissingAndCorruptFile(t *testing.T) {
	// Missing file: discovery will produce the state, so this is not an error.
	w := stateWatcher(t, filepath.Join(t.TempDir(), "does-not-exist.json"))
	if len(w.variants) != 0 {
		t.Errorf("got %d variants from a missing file", len(w.variants))
	}

	// Corrupt file must not panic.
	path := filepath.Join(t.TempDir(), "hls-source.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	w = stateWatcher(t, path)
	if len(w.variants) != 0 {
		t.Errorf("got %d variants from a corrupt file", len(w.variants))
	}
}
