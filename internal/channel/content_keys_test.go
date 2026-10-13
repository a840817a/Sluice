package channel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testKeyA = "00112233445566778899aabbccddeeff:ffeeddccbbaa99887766554433221100"
	testKeyB = "10112233445566778899aabbccddeeff:0f0e0d0c0b0a09080706050403020100"
	// testKeyAOtherValue has testKeyA's KID and a different key.
	testKeyAOtherValue = "00112233445566778899aabbccddeeff:00000000000000000000000000000001"
)

func TestValidateContentKeyRules(t *testing.T) {
	const url = "http://example.com/live.mpd"
	cases := []struct {
		name    string
		cfg     Config
		gate    bool   // dashDecryptEnabled during the case
		wantErr string // substring; "" means accepted
	}{
		{"dash decrypt is gated", Config{MPDURL: url, KeyMode: KeyModeDecrypt, ContentKeys: []string{testKeyA}},
			false, "DASH decrypt is not supported yet"},
		{"empty source type counts as dash and is gated", Config{MPDURL: url, KeyMode: KeyModeDecrypt},
			false, "DASH decrypt is not supported yet"},
		{"dash decrypt with a key", Config{MPDURL: url, KeyMode: KeyModeDecrypt, ContentKeys: []string{testKeyA}},
			true, ""},
		{"dash decrypt with two keys", Config{MPDURL: url, SourceType: SourceDASH, KeyMode: KeyModeDecrypt,
			ContentKeys: []string{testKeyA, testKeyB}}, true, ""},
		{"dash decrypt with upper-case hex", Config{MPDURL: url, KeyMode: KeyModeDecrypt,
			ContentKeys: []string{strings.ToUpper(testKeyA)}}, true, ""},
		{"dash decrypt needs a key", Config{MPDURL: url, KeyMode: KeyModeDecrypt},
			true, "needs at least one key"},
		{"dash decrypt malformed key", Config{MPDURL: url, KeyMode: KeyModeDecrypt, ContentKeys: []string{"abc"}},
			true, "content_keys[0]"},
		{"dash decrypt short hex", Config{MPDURL: url, KeyMode: KeyModeDecrypt,
			ContentKeys: []string{testKeyA, "0011:2233"}}, true, "content_keys[1]"},
		{"duplicate KID with a different key", Config{MPDURL: url, KeyMode: KeyModeDecrypt,
			ContentKeys: []string{testKeyA, testKeyAOtherValue}}, true, "duplicate KID"},
		{"duplicate KID differing only in case", Config{MPDURL: url, KeyMode: KeyModeDecrypt,
			ContentKeys: []string{testKeyA, strings.ToUpper(testKeyA)}}, true, "duplicate KID"},
		{"dash passthrough with keys", Config{MPDURL: url, KeyMode: KeyModePassthrough, ContentKeys: []string{testKeyA}},
			true, "only allowed for a dash source with key_mode decrypt"},
		{"dash default mode with keys", Config{MPDURL: url, ContentKeys: []string{testKeyA}},
			true, "only allowed for a dash source with key_mode decrypt"},
		{"hls decrypt with keys", Config{MPDURL: url, SourceType: SourceHLS, KeyMode: KeyModeDecrypt,
			ContentKeys: []string{testKeyA}}, true, "only allowed for a dash source with key_mode decrypt"},
		{"hls passthrough with keys", Config{MPDURL: url, SourceType: SourceHLS, KeyMode: KeyModePassthrough,
			ContentKeys: []string{testKeyA}}, true, "only allowed for a dash source with key_mode decrypt"},
		{"hls decrypt without keys stays valid", Config{MPDURL: url, SourceType: SourceHLS, KeyMode: KeyModeDecrypt},
			false, ""},
		{"dash passthrough stays valid", Config{MPDURL: url, KeyMode: KeyModePassthrough}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := dashDecryptEnabled
			dashDecryptEnabled = tc.gate
			defer func() { dashDecryptEnabled = old }()
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			// An error must never echo key digits.
			for _, k := range tc.cfg.ContentKeys {
				if _, v, _ := strings.Cut(strings.ToLower(k), ":"); len(v) == 32 && strings.Contains(strings.ToLower(err.Error()), v) {
					t.Errorf("error %q contains a key value", err)
				}
			}
		})
	}
}

func TestNormalizeKeyAlias(t *testing.T) {
	cases := []struct {
		name    string
		in      Config
		want    string
		wantErr bool
	}{
		{"nothing", Config{}, "", false},
		{"new name only", Config{KeyMode: KeyModeDecrypt}, KeyModeDecrypt, false},
		{"old name only", Config{LegacyHLSKeyMode: KeyModeDecrypt}, KeyModeDecrypt, false},
		{"both agree", Config{KeyMode: KeyModeDecrypt, LegacyHLSKeyMode: KeyModeDecrypt}, KeyModeDecrypt, false},
		{"both disagree", Config{KeyMode: KeyModePassthrough, LegacyHLSKeyMode: KeyModeDecrypt}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.in
			err := cfg.NormalizeKeyAlias()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (cfg.KeyMode != tc.want || cfg.LegacyHLSKeyMode != "") {
				t.Errorf("got KeyMode=%q legacy=%q, want %q and empty", cfg.KeyMode, cfg.LegacyHLSKeyMode, tc.want)
			}
		})
	}
}

// A channels.json written before key_mode existed still loads, and the next
// save writes the new name only.
func TestStoreLoadsLegacyHLSKeyModeAndSavesNewName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.json")
	legacy := `{"old":{"mpd_url":"http://o/m.m3u8","source_type":"hls","hls_key_mode":"decrypt","enabled":true},
		"plain":{"mpd_url":"http://o/a.mpd","enabled":true}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	got, _ := s.Get("old")
	if got.KeyMode != KeyModeDecrypt || !got.DecryptOnIngest() {
		t.Errorf("legacy channel KeyMode = %q, want decrypt", got.KeyMode)
	}

	if _, err := s.Update("plain", func(c *Config) { c.Title = "t" }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hls_key_mode") {
		t.Errorf("saved file still uses the old name:\n%s", data)
	}
	var onDisk map[string]map[string]any
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["old"]["key_mode"] != "decrypt" {
		t.Errorf("saved key_mode = %v, want decrypt", onDisk["old"]["key_mode"])
	}
}

func TestStoreRejectsConflictingKeyModeNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.json")
	body := `{"x":{"mpd_url":"http://o/a.mpd","key_mode":"passthrough","hls_key_mode":"decrypt"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil {
		t.Fatal("NewStore accepted a file naming two different key modes")
	}
}

func TestStoreKeepsContentKeysAndClonesThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := Config{MPDURL: "http://o/a.mpd", KeyMode: KeyModeDecrypt, ContentKeys: []string{testKeyA}}
	if err := s.Put("c", in); err != nil {
		t.Fatal(err)
	}
	in.ContentKeys[0] = "mutated"
	got, _ := s.Get("c")
	if got.ContentKeys[0] != testKeyA {
		t.Errorf("stored key aliased the caller's slice: %q", got.ContentKeys[0])
	}
	got.ContentKeys[0] = "mutated"
	again, _ := s.Get("c")
	if again.ContentKeys[0] != testKeyA {
		t.Error("Get returned the store's own slice")
	}
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reload, _ := s2.Get("c"); len(reload.ContentKeys) != 1 || reload.ContentKeys[0] != testKeyA {
		t.Errorf("reloaded ContentKeys = %v", reload.ContentKeys)
	}
}

// An edit replaces what the operator owns, but not the key mode or the keys.
func TestMergedOperatorConfigPreservesKeyModeAndKeys(t *testing.T) {
	current := Config{MPDURL: "http://o/a.mpd", KeyMode: KeyModeDecrypt, ContentKeys: []string{testKeyA}}
	requested := Config{MPDURL: "http://o/b.mpd", KeyMode: KeyModePassthrough,
		LegacyHLSKeyMode: "x", ContentKeys: []string{testKeyB}, Title: "new"}
	got := mergedOperatorConfig(requested, current, false)
	if got.KeyMode != KeyModeDecrypt || got.LegacyHLSKeyMode != "" {
		t.Errorf("KeyMode = %q legacy = %q, want decrypt and empty", got.KeyMode, got.LegacyHLSKeyMode)
	}
	if len(got.ContentKeys) != 1 || got.ContentKeys[0] != testKeyA {
		t.Errorf("ContentKeys = %v, want the stored key only", got.ContentKeys)
	}
	if got.Title != "new" || got.MPDURL != "http://o/b.mpd" {
		t.Error("operator-owned fields were not applied")
	}
	got.ContentKeys[0] = "mutated"
	if current.ContentKeys[0] != testKeyA {
		t.Error("merged config aliases the stored key slice")
	}
}

func TestPublicConfigHidesKeyValues(t *testing.T) {
	cfg := Config{MPDURL: "http://o/a.mpd", KeyMode: KeyModeDecrypt, ContentKeys: []string{testKeyA, testKeyB}}
	b, err := json.Marshal(cfg.Public())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{testKeyA, testKeyB} {
		_, val, _ := strings.Cut(k, ":")
		if strings.Contains(string(b), val) {
			t.Errorf("public config contains key value %s: %s", val, b)
		}
	}
	if !strings.Contains(string(b), `"content_key_ids":["00112233445566778899aabbccddeeff","10112233445566778899aabbccddeeff"]`) {
		t.Errorf("public config lacks content_key_ids: %s", b)
	}
	if strings.Contains(string(b), `"content_keys"`) {
		t.Errorf("public config has a content_keys field: %s", b)
	}
	if len(cfg.ContentKeys) != 2 {
		t.Error("Public mutated its receiver")
	}
}
