package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/go-chi/chi/v5"
)

const (
	keyA = "00112233445566778899aabbccddeeff:ffeeddccbbaa99887766554433221100"
	keyB = "10112233445566778899aabbccddeeff:0f0e0d0c0b0a09080706050403020100"
	keyC = "20112233445566778899aabbccddeeff:1f1e1d1c1b1a19181716151413121110"
	// keyAAgain repeats keyA's KID under a different key value.
	keyAAgain = "00112233445566778899aabbccddeeff:00000000000000000000000000000009"
)

// keyValue is the part of "kid:key" that must never leave the gateway.
func keyValue(k string) string { _, v, _ := strings.Cut(k, ":"); return v }
func keyKID(k string) string   { kid, _, _ := strings.Cut(k, ":"); return kid }

// dashDecryptChannel is a stored DASH channel in decrypt mode. Tests write it
// straight to the store when the channel need not run.
func dashDecryptChannel(keys ...string) ch.Config {
	return ch.Config{
		MPDURL:      "http://127.0.0.1:1/live.mpd",
		KeyMode:     ch.KeyModeDecrypt,
		ContentKeys: keys,
		Enabled:     true,
	}
}

func postKeys(admin *AdminServer, id string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		_ = json.NewEncoder(&buf).Encode(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/channels/"+id+"/keys", &buf)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channelID", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	admin.handleAddKeys(w, req)
	return w
}

func storedKeys(t *testing.T, admin *AdminServer, id string) []string {
	t.Helper()
	cfg, ok := storeOf(t, admin).Get(id)
	if !ok {
		t.Fatalf("channel %q missing from the store", id)
	}
	return cfg.ContentKeys
}

// assertNoKeyValues fails if body holds the value half of any key.
func assertNoKeyValues(t *testing.T, what, body string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if strings.Contains(strings.ToLower(body), keyValue(k)) {
			t.Errorf("%s contains a key value (%s...):\n%s", what, keyValue(k)[:6], body)
		}
	}
}

func TestAdminListNeverShowsKeyValues(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := storeOf(t, admin).Put("d", dashDecryptChannel(keyA, keyB)); err != nil {
		t.Fatal(err)
	}
	w := adminCall(admin.handleListChannels, http.MethodGet, "/admin/api/channels", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	assertNoKeyValues(t, "channel list", body, keyA, keyB)
	if strings.Contains(body, `"content_keys"`) {
		t.Errorf("channel list has a content_keys field:\n%s", body)
	}
	var got map[string]struct {
		Config struct {
			KeyMode       string   `json:"key_mode"`
			ContentKeyIDs []string `json:"content_key_ids"`
		} `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{keyKID(keyA), keyKID(keyB)}
	if !slices.Equal(got["d"].Config.ContentKeyIDs, want) || got["d"].Config.KeyMode != "decrypt" {
		t.Errorf("config = %+v, want key_mode decrypt and content_key_ids %v", got["d"].Config, want)
	}
}

func TestAdminCreateAcceptsLegacyHLSKeyMode(t *testing.T) {
	admin := newTestAdminServer(t)
	t.Cleanup(admin.manager.StopAll)

	w := adminExec(admin, http.MethodPost, "/admin/api/channels", map[string]any{
		"id": "legacy", "mpd_url": "http://127.0.0.1:1/m.m3u8", "source_type": "hls", "hls_key_mode": "decrypt",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with the old name: %d %s", w.Code, w.Body.String())
	}
	got, _ := storeOf(t, admin).Get("legacy")
	if got.KeyMode != ch.KeyModeDecrypt || got.LegacyHLSKeyMode != "" {
		t.Errorf("stored KeyMode = %q legacy = %q, want decrypt and empty", got.KeyMode, got.LegacyHLSKeyMode)
	}

	for name, body := range map[string]map[string]any{
		"names disagree": {"id": "bad", "mpd_url": "http://127.0.0.1:1/m.m3u8", "source_type": "hls",
			"hls_key_mode": "decrypt", "key_mode": "passthrough"},
		"unknown value": {"id": "bad", "mpd_url": "http://127.0.0.1:1/m.m3u8", "hls_key_mode": "sample-aes"},
		"keys on an hls channel": {"id": "bad", "mpd_url": "http://127.0.0.1:1/m.m3u8", "source_type": "hls",
			"content_keys": []string{keyA}},
	} {
		t.Run(name, func(t *testing.T) {
			w := adminExec(admin, http.MethodPost, "/admin/api/channels", body, nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", w.Code, w.Body.String())
			}
			assertNoKeyValues(t, "error body", w.Body.String(), keyA)
		})
	}
	if admin.manager.HasChannel("bad") {
		t.Error("a rejected create left a channel behind")
	}
}

// Creating a DASH decrypt channel checks its keys: at least one, each
// "kid:key" in hex, no KID twice. The error never echoes a key value.
func TestAdminCreateDASHDecryptChecksKeys(t *testing.T) {
	admin := newTestAdminServer(t)
	t.Cleanup(admin.manager.StopAll)
	create := func(id string, keys []string) *httptest.ResponseRecorder {
		return adminExec(admin, http.MethodPost, "/admin/api/channels", map[string]any{
			"id": id, "mpd_url": "http://127.0.0.1:1/m.mpd", "key_mode": "decrypt", "content_keys": keys,
		}, nil)
	}

	for name, keys := range map[string][]string{
		"no keys":           nil,
		"malformed key":     {keyA, "0011:" + keyValue(keyB)},
		"key not hex":       {keyKID(keyA) + ":" + strings.Repeat("zz", 16)},
		"duplicate KID":     {keyA, keyAAgain},
		"duplicate in case": {keyA, strings.ToUpper(keyA)},
	} {
		t.Run(name, func(t *testing.T) {
			w := create("bad", keys)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", w.Code, w.Body.String())
			}
			assertNoKeyValues(t, "error body", w.Body.String(), append(keys, keyA, keyB, keyAAgain)...)
		})
	}
	if admin.manager.HasChannel("bad") {
		t.Error("a rejected create left a channel behind")
	}

	w := create("good", []string{keyA, keyB})
	if w.Code != http.StatusCreated {
		t.Fatalf("create with two keys: %d %s", w.Code, w.Body.String())
	}
	assertNoKeyValues(t, "create response", w.Body.String(), keyA, keyB)
	if got := storedKeys(t, admin, "good"); !slices.Equal(got, []string{keyA, keyB}) {
		t.Errorf("stored keys = %d entries, want the two given", len(got))
	}
}

func TestAdminUpdateKeyModeIsImmutable(t *testing.T) {
	admin := newTestAdminServer(t)
	cfg := liveChannel()
	cfg.KeyMode = ch.KeyModeDecrypt
	if _, err := createChannel(admin.manager, context.Background(), "c", cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.manager.StopAll)
	put := func(extra map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"mpd_url": cfg.MPDURL, "source_type": "hls"}
		for k, v := range extra {
			body[k] = v
		}
		return adminExec(admin, http.MethodPut, "/admin/api/channels/c", body, map[string]string{"channelID": "c"})
	}

	for name, extra := range map[string]map[string]any{
		"to passthrough":     {"key_mode": "passthrough"},
		"old name":           {"hls_key_mode": "passthrough"},
		"cannot be unlocked": {"key_mode": "decrypt", "hls_key_mode": "passthrough"},
	} {
		if w := put(extra); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	if got, _ := storeOf(t, admin).Get("c"); got.KeyMode != ch.KeyModeDecrypt {
		t.Errorf("stored KeyMode = %q after rejected edits, want decrypt", got.KeyMode)
	}
	// Repeating the current mode, or leaving it out, is not a change.
	for name, extra := range map[string]map[string]any{
		"same value": {"key_mode": "decrypt", "title": "a"},
		"omitted":    {"title": "b"},
	} {
		if w := put(extra); w.Code != http.StatusOK {
			t.Errorf("%s: got %d %s, want 200", name, w.Code, w.Body.String())
		}
	}
	if got, _ := storeOf(t, admin).Get("c"); got.KeyMode != ch.KeyModeDecrypt {
		t.Errorf("stored KeyMode = %q after an edit that omitted it, want decrypt", got.KeyMode)
	}
}

func TestAdminUpdateKeepsStoredContentKeys(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"keys omitted":   {},
		"keys replaced":  {"content_keys": []string{keyC}},
		"keys emptied":   {"content_keys": []string{}},
		"keys malformed": {"content_keys": []string{"nonsense"}},
		"mode repeated":  {"key_mode": "decrypt", "content_keys": []string{keyB}},
	} {
		t.Run(name, func(t *testing.T) {
			admin := newTestAdminServer(t)
			if err := storeOf(t, admin).Put("d", dashDecryptChannel(keyA)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(admin.manager.StopAll)
			body["mpd_url"] = "http://127.0.0.1:1/other.mpd"
			body["title"] = "edited"
			w := adminExec(admin, http.MethodPut, "/admin/api/channels/d", body, map[string]string{"channelID": "d"})
			if w.Code != http.StatusOK {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			got, _ := storeOf(t, admin).Get("d")
			if !slices.Equal(got.ContentKeys, []string{keyA}) || got.KeyMode != ch.KeyModeDecrypt || got.Title != "edited" {
				t.Errorf("stored = keys %v mode %q title %q, want the original keys and mode with the new title",
					got.ContentKeys, got.KeyMode, got.Title)
			}
		})
	}
}

func TestAdminUpdateSourceTypeCannotStrandKeys(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := storeOf(t, admin).Put("d", dashDecryptChannel(keyA)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.manager.StopAll)
	w := adminExec(admin, http.MethodPut, "/admin/api/channels/d",
		map[string]any{"mpd_url": "http://127.0.0.1:1/m.m3u8", "source_type": "hls"}, map[string]string{"channelID": "d"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("dash decrypt -> hls: got %d %s, want 400", w.Code, w.Body.String())
	}
	assertNoKeyValues(t, "error body", w.Body.String(), keyA)
	if got, _ := storeOf(t, admin).Get("d"); got.SourceType != "" {
		t.Errorf("rejected edit changed SourceType to %q", got.SourceType)
	}
}

// recreateHint is what the 400 of an edit that would change a setting fixed
// at creation tells the operator to do instead.
const recreateHint = "delete the channel and create it again"

// Both edits a decrypt channel cannot take, a new key_mode and a source type
// that strands its keys, say in their 400 how to get there instead.
func TestAdminFixedSettingErrorsSayRecreate(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := storeOf(t, admin).Put("d", dashDecryptChannel(keyA)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.manager.StopAll)
	for name, body := range map[string]map[string]any{
		"key_mode":    {"mpd_url": "http://127.0.0.1:1/m.mpd", "key_mode": "passthrough"},
		"source_type": {"mpd_url": "http://127.0.0.1:1/m.m3u8", "source_type": "hls"},
	} {
		w := adminExec(admin, http.MethodPut, "/admin/api/channels/d", body, map[string]string{"channelID": "d"})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), recreateHint) {
			t.Errorf("%s edit: got %d %s, want 400 containing %q", name, w.Code, w.Body.String(), recreateHint)
		}
	}
}

func TestAdminAddKeys(t *testing.T) {
	type want struct {
		code int
		kids []string // added_kids on 200
		keys []string // stored keys afterwards
	}
	cases := []struct {
		name string
		seed ch.Config
		body any
		want want
	}{
		{"adds keys", dashDecryptChannel(keyA), map[string]any{"content_keys": []string{keyB, keyC}},
			want{http.StatusOK, []string{keyKID(keyB), keyKID(keyC)}, []string{keyA, keyB, keyC}}},
		{"upper-case input is stored normalised", dashDecryptChannel(keyA),
			map[string]any{"content_keys": []string{strings.ToUpper(keyB)}},
			want{http.StatusOK, []string{keyKID(keyB)}, []string{keyA, keyB}}},
		{"existing KID is a conflict", dashDecryptChannel(keyA), map[string]any{"content_keys": []string{keyAAgain}},
			want{http.StatusConflict, nil, []string{keyA}}},
		// Atomic: the new key is not stored because a later one conflicts.
		{"conflict adds nothing", dashDecryptChannel(keyA), map[string]any{"content_keys": []string{keyB, keyAAgain, keyC}},
			want{http.StatusConflict, nil, []string{keyA}}},
		{"duplicate within the request", dashDecryptChannel(keyA), map[string]any{"content_keys": []string{keyB, keyB}},
			want{http.StatusBadRequest, nil, []string{keyA}}},
		{"malformed key adds nothing", dashDecryptChannel(keyA), map[string]any{"content_keys": []string{keyB, "bogus"}},
			want{http.StatusBadRequest, nil, []string{keyA}}},
		{"empty list", dashDecryptChannel(keyA), map[string]any{"content_keys": []string{}},
			want{http.StatusBadRequest, nil, []string{keyA}}},
		{"no list", dashDecryptChannel(keyA), map[string]any{},
			want{http.StatusBadRequest, nil, []string{keyA}}},
		{"not json", dashDecryptChannel(keyA), "{",
			want{http.StatusBadRequest, nil, []string{keyA}}},
		{"dash passthrough channel", ch.Config{MPDURL: "http://127.0.0.1:1/a.mpd", Enabled: true},
			map[string]any{"content_keys": []string{keyB}}, want{http.StatusBadRequest, nil, nil}},
		{"hls decrypt channel", ch.Config{MPDURL: "http://127.0.0.1:1/a.m3u8", SourceType: ch.SourceHLS,
			KeyMode: ch.KeyModeDecrypt, Enabled: true},
			map[string]any{"content_keys": []string{keyB}}, want{http.StatusBadRequest, nil, nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admin := newTestAdminServer(t)
			if err := storeOf(t, admin).Put("d", tc.seed); err != nil {
				t.Fatal(err)
			}
			w := postKeys(admin, "d", tc.body)
			if w.Code != tc.want.code {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.want.code)
			}
			assertNoKeyValues(t, "response", w.Body.String(), keyA, keyB, keyC, keyAAgain)
			if tc.want.code == http.StatusOK {
				var resp struct {
					AddedKIDs []string `json:"added_kids"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(resp.AddedKIDs, tc.want.kids) {
					t.Errorf("added_kids = %v, want %v", resp.AddedKIDs, tc.want.kids)
				}
			}
			if got := storedKeys(t, admin, "d"); !slices.Equal(got, tc.want.keys) {
				t.Errorf("stored keys = %v, want %v", got, tc.want.keys)
			}
		})
	}
}

func TestAdminAddKeysUnknownChannel(t *testing.T) {
	admin := newTestAdminServer(t)
	if w := postKeys(admin, "nope", map[string]any{"content_keys": []string{keyA}}); w.Code != http.StatusNotFound {
		t.Errorf("got %d %s, want 404", w.Code, w.Body.String())
	}
}

// Added keys reach the running channel without a restart, and survive the
// next edit.
func TestAdminAddKeysHotAppliesToRunningChannel(t *testing.T) {
	admin := newTestAdminServer(t)
	if err := storeOf(t, admin).Put("d", dashDecryptChannel(keyA)); err != nil {
		t.Fatal(err)
	}
	if err := admin.manager.Restart(context.Background(), "d"); err != nil {
		t.Fatalf("start: %v", err)
	}
	rt := admin.manager.Get("d")
	t.Cleanup(admin.manager.StopAll)

	if w := postKeys(admin, "d", map[string]any{"content_keys": []string{keyB}}); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if now := admin.manager.Get("d"); now != rt {
		t.Error("adding keys replaced the runtime")
	}
	if got := rt.Config().ContentKeys; !slices.Equal(got, []string{keyA, keyB}) {
		t.Errorf("running config keys = %v, want both", got)
	}

	w := adminExec(admin, http.MethodPut, "/admin/api/channels/d",
		map[string]any{"mpd_url": "http://127.0.0.1:1/live.mpd", "title": "x"}, map[string]string{"channelID": "d"})
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if got := storedKeys(t, admin, "d"); !slices.Equal(got, []string{keyA, keyB}) {
		t.Errorf("stored keys after an edit = %v, want both", got)
	}
	if got := admin.manager.Get("d").Config().ContentKeys; !slices.Equal(got, []string{keyA, keyB}) {
		t.Errorf("running keys after an edit = %v, want both", got)
	}
}
