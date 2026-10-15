package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/cenc/cenctest"
	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/store"
)

// cencRotationKey is the key the live origin's later video segments rotate
// to with a seig sample group of their own; no init names its KID.
const cencRotationKey = "40404040404040404040404040404040:404142434445464748494a4b4c4d4e4f"

// rotatedVideo is clear video segment n encrypted as a cenc origin that
// rotated its key would serve it: every sample in the fragment's own seig
// group under cencRotationKey's KID.
func rotatedVideo(t *testing.T, o *cencOrigin, n int) []byte {
	t.Helper()
	video := mustParseKey(t, cencVideoKey)
	rot := mustParseKey(t, cencRotationKey)
	clear := o.fixture(t, "clear", cencReps[0], fmt.Sprintf("seg-%d.m4s", n))
	index := make([]uint32, 12)
	for i := range index {
		index[i] = cenctest.LocalBase + 1
	}
	seg, err := cenctest.Rotate(clear, cenctest.Rotation{
		Default: cenctest.Group{Protected: true, IVSize: 16, KID: video.KID},
		Local:   []cenctest.Group{{Protected: true, IVSize: 8, KID: rot.KID}},
		Index:   index,
		Keys:    map[[16]byte][16]byte{video.KID: video.Value, rot.KID: rot.Value},
	})
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

func mustParseKey(t *testing.T, s string) cenc.Key {
	t.Helper()
	k, err := cenc.ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// servedClear reports whether the gateway serves video segment n with the
// clear fixture's samples.
func servedClear(t *testing.T, h http.Handler, o *cencOrigin, n int) bool {
	t.Helper()
	code, seg := get(t, h, segmentURL(cencReps[0], n))
	if code != http.StatusOK {
		return false
	}
	clear := o.fixture(t, "clear", cencReps[0], fmt.Sprintf("seg-%d.m4s", n))
	if !bytes.Equal(mdatOf(t, seg), mdatOf(t, clear)) {
		t.Fatalf("video segment %d served with samples that differ from the clear fixture's", n)
	}
	return true
}

func missingKeyWarning(ws []dashdecrypt.Warning) (dashdecrypt.Warning, bool) {
	for _, w := range ws {
		if w.Kind == dashdecrypt.KindMissingKey {
			return w, true
		}
	}
	return dashdecrypt.Warning{}, false
}

// A live origin whose video segments 3 and 4 rotate to a KID no init names.
// With that key held they are decrypted like the rest. Without it they are
// withheld, the AdaptationSet stays clear and missing_key lists the KID;
// once the key is added the next poll fetches them again and the warning
// clears.
func TestDASHDecryptKeyRotation(t *testing.T) {
	rotKID := mustParseKey(t, cencRotationKey).KID.String()
	start := func(t *testing.T, keys ...string) (*cencOrigin, http.Handler, func() []dashdecrypt.Warning, func(context.Context, []string) error) {
		o := startCENCOrigin(t, "cenc", liveCENC(false))
		for _, n := range []int{3, 4} {
			o.extra.Store(fmt.Sprintf("v/seg-%d.m4s", n), rotatedVideo(t, o, n))
		}
		mgr, h := newCENCChannel(t, o, t.TempDir(), keys...)
		warnings := func() []dashdecrypt.Warning { return decryptWarnings(t, mgr) }
		add := func(ctx context.Context, k []string) error {
			_, err := mgr.AddContentKeys(ctx, "c", k)
			return err
		}
		return o, h, warnings, add
	}

	t.Run("key held", func(t *testing.T) {
		o, h, warnings, _ := start(t, cencVideoKey, cencAudioKey, cencRotationKey)
		waitFor(t, "video segment 4 to be served", func() bool { return servedClear(t, h, o, 4) })
		for n := 1; n <= 4; n++ {
			if !servedClear(t, h, o, n) {
				t.Errorf("video segment %d not served", n)
			}
		}
		if ws := warnings(); len(ws) != 0 {
			t.Errorf("decrypt warnings = %+v, want none", ws)
		}
	})

	t.Run("key added later", func(t *testing.T) {
		o, h, warnings, add := start(t, cencVideoKey, cencAudioKey)
		waitFor(t, "missing_key for the rotation KID", func() bool {
			w, ok := missingKeyWarning(warnings())
			return ok && slices.Contains(w.KIDs, rotKID)
		})
		waitFor(t, "video segment 2 to be served", func() bool { return servedClear(t, h, o, 2) })
		w, _ := missingKeyWarning(warnings())
		if w.AdaptationSet != "0" || !slices.Equal(w.KIDs, []string{rotKID}) {
			t.Errorf("missing_key = %+v, want the video AdaptationSet and only %s", w, rotKID)
		}
		// Give the later polls time to show nothing changes without the key.
		time.Sleep(1500 * time.Millisecond)
		for _, n := range []int{3, 4} {
			if code, _ := get(t, h, segmentURL(cencReps[0], n)); code == http.StatusOK {
				t.Errorf("video segment %d served without its key", n)
			}
		}
		if code, _ := get(t, h, "/v1/channels/c/manifest.mpd"); code != http.StatusOK {
			t.Errorf("manifest: %d", code)
		}

		if err := add(context.Background(), []string{cencRotationKey}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "video segments 3 and 4 to be served after the key was added", func() bool {
			return servedClear(t, h, o, 3) && servedClear(t, h, o, 4)
		})
		waitFor(t, "the missing_key warning to clear", func() bool {
			_, ok := missingKeyWarning(warnings())
			return !ok
		})
		if ws := warnings(); len(ws) != 0 {
			t.Errorf("decrypt warnings = %+v, want none", ws)
		}
	})
}

// postKey adds key to channel "c" through the admin API.
func postKey(t *testing.T, mgr *ch.Manager, key string) {
	t.Helper()
	w := postKeys(NewAdminServer(context.Background(), mgr), "c", map[string][]string{"content_keys": {key}})
	if w.Code != http.StatusOK {
		t.Fatalf("POST keys: %d %s", w.Code, w.Body)
	}
}

// rotatingStaticOrigin is a static origin whose video segments 3 and 4
// rotate to cencRotationKey's KID, and whose last audio segment is held, so
// the channel keeps ingesting until the test releases it.
func rotatingStaticOrigin(t *testing.T) (*cencOrigin, func()) {
	o := startCENCOrigin(t, "cenc", nil)
	for _, n := range []int{3, 4} {
		o.extra.Store(fmt.Sprintf("v/seg-%d.m4s", n), rotatedVideo(t, o, n))
	}
	return o, o.hold(t, "a/seg-5.m4s")
}

// waitWithheld waits until video segments 3 and 4 were fetched and the
// rotation KID is reported missing.
func waitWithheld(t *testing.T, o *cencOrigin, warnings func() []dashdecrypt.Warning) {
	t.Helper()
	rotKID := mustParseKey(t, cencRotationKey).KID.String()
	waitFor(t, "video segments 3 and 4 to be withheld", func() bool {
		w, ok := missingKeyWarning(warnings())
		return ok && slices.Contains(w.KIDs, rotKID) && o.requests("v/seg-3.m4s") > 0 && o.requests("v/seg-4.m4s") > 0
	})
	// Both fetched; give the processor a moment to have seen both.
	time.Sleep(300 * time.Millisecond)
}

// A static origin's segments withheld for a rotation key are fetched again
// once the key is added while the channel still ingests, and the VOD it
// becomes holds them clear. Stopped instead, and restarted after the key
// was added, the channel enumerates the asset again and fetches them too.
func TestDASHDecryptStaticKeyRotation(t *testing.T) {
	t.Run("key added while ingesting", func(t *testing.T) {
		o, release := rotatingStaticOrigin(t)
		mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey, cencAudioKey)
		warnings := func() []dashdecrypt.Warning { return decryptWarnings(t, mgr) }
		waitWithheld(t, o, warnings)

		postKey(t, mgr, cencRotationKey)
		waitFor(t, "video segments 3 and 4 to be fetched again", func() bool {
			return o.requests("v/seg-3.m4s") > 1 && o.requests("v/seg-4.m4s") > 1
		})
		release()
		waitVOD(t, mgr)
		for n := 1; n <= 4; n++ {
			if !servedClear(t, h, o, n) {
				t.Errorf("video segment %d not served", n)
			}
		}
		if ws := warnings(); len(ws) != 0 {
			t.Errorf("decrypt warnings = %+v, want none", ws)
		}
	})

	t.Run("key added while stopped", func(t *testing.T) {
		o, release := rotatingStaticOrigin(t)
		mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey, cencAudioKey)
		warnings := func() []dashdecrypt.Warning { return decryptWarnings(t, mgr) }
		waitWithheld(t, o, warnings)
		if err := mgr.Stop("c"); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.AddContentKeys(context.Background(), "c", []string{cencRotationKey}); err != nil {
			t.Fatal(err)
		}
		release()
		if err := mgr.Restart(context.Background(), "c"); err != nil {
			t.Fatal(err)
		}
		waitVOD(t, mgr)
		for n := 1; n <= 4; n++ {
			if !servedClear(t, h, o, n) {
				t.Errorf("video segment %d not served", n)
			}
		}
		if ws := warnings(); len(ws) != 0 {
			t.Errorf("decrypt warnings = %+v, want none", ws)
		}
	})
}

// The procedure the README gives for a VOD: segments withheld before the
// transition stay holes when the key is added, and a restore to live
// fetches them, so the next transition holds them.
func TestDASHDecryptStaticKeyRotationAfterVOD(t *testing.T) {
	o, release := rotatingStaticOrigin(t)
	release()
	mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey, cencAudioKey)
	waitVOD(t, mgr)
	if _, ok := missingKeyWarning(decryptWarnings(t, mgr)); !ok {
		t.Fatalf("warnings = %+v, want missing_key", decryptWarnings(t, mgr))
	}
	postKey(t, mgr, cencRotationKey)
	time.Sleep(300 * time.Millisecond)
	for _, n := range []int{3, 4} {
		if code, _ := get(t, h, segmentURL(cencReps[0], n)); code == http.StatusOK {
			t.Errorf("video segment %d served by a VOD that ingested nothing", n)
		}
	}

	if err := mgr.RestoreToLive(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "video segments 3 and 4 to be served", func() bool { return servedClear(t, h, o, 3) && servedClear(t, h, o, 4) })
	if err := mgr.TransitionToVOD("c"); err != nil {
		t.Fatal(err)
	}
	waitVOD(t, mgr)
	for n := 1; n <= 4; n++ {
		if !servedClear(t, h, o, n) {
			t.Errorf("video segment %d not served by the new VOD", n)
		}
	}
	if ws := decryptWarnings(t, mgr); len(ws) != 0 {
		t.Errorf("decrypt warnings = %+v, want none", ws)
	}
}

// A live channel restarted while segments are withheld reads the withheld
// KID back from decrypt.json: the missing_key warning survives even though
// the origin no longer lists those segments, and adding the key ends it
// although they can no longer be fetched.
func TestDASHDecryptWithheldSurvivesRestart(t *testing.T) {
	rotKID := mustParseKey(t, cencRotationKey).KID.String()
	o := startCENCOrigin(t, "cenc", liveCENC(false))
	for _, n := range []int{3, 4} {
		o.extra.Store(fmt.Sprintf("v/seg-%d.m4s", n), rotatedVideo(t, o, n))
	}
	dataDir := t.TempDir()
	mgr, _ := newCENCChannel(t, o, dataDir, cencVideoKey, cencAudioKey)
	warnings := func() []dashdecrypt.Warning { return decryptWarnings(t, mgr) }
	waitWithheld(t, o, warnings)
	if err := mgr.Stop("c"); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(store.RepLocation{DataDir: dataDir, ChannelID: "c", MediaType: "video", ASID: "0", RepID: "v"}.ASDir(),
		dashdecrypt.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(state, []byte(rotKID)) {
		t.Errorf("%s does not record the withheld KID:\n%s", dashdecrypt.StateFileName, state)
	}

	// The segments have left the origin's window.
	o.setEdit(liveCENC(true))
	if err := mgr.Restart(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	w, ok := missingKeyWarning(warnings())
	if !ok || w.AdaptationSet != "0" || !slices.Equal(w.KIDs, []string{rotKID}) {
		t.Fatalf("after the restart: warnings %+v, want missing_key for %s", warnings(), rotKID)
	}

	postKey(t, mgr, cencRotationKey)
	if w, ok := missingKeyWarning(warnings()); ok {
		t.Errorf("missing_key = %+v after its key was added", w)
	}
}

// A ciphertext AdaptationSet whose stored segments rotate to a KID without
// a key is not converted when its init's key arrives: missing_key names the
// rotation KID and the AdaptationSet stays as stored. An unrelated key
// changes nothing; the rotation key converts it.
func TestDASHDecryptConversionMissingRotationKey(t *testing.T) {
	rotKID := mustParseKey(t, cencRotationKey).KID.String()
	o := startCENCOrigin(t, "cenc", nil)
	for _, n := range []int{3, 4} {
		o.extra.Store(fmt.Sprintf("v/seg-%d.m4s", n), rotatedVideo(t, o, n))
	}
	mgr, h := newCENCChannel(t, o, t.TempDir(), cencAudioKey)
	waitVOD(t, mgr)
	video := cencReps[0]

	onlyRotationMissing := func() bool {
		ws := decryptWarnings(t, mgr)
		w, ok := missingKeyWarning(ws)
		return ok && w.AdaptationSet == "0" && slices.Equal(w.KIDs, []string{rotKID}) &&
			!slices.ContainsFunc(ws, func(w dashdecrypt.Warning) bool { return w.Kind == dashdecrypt.KindConversionFailed })
	}
	stillCiphertext := func() {
		t.Helper()
		if code, init := get(t, h, initURL(video)); code != http.StatusOK || !bytes.Equal(init, o.fixture(t, "cenc", video, "init.mp4")) {
			t.Errorf("video init: %d, want 200 with the origin's bytes", code)
		}
		if code, seg := get(t, h, segmentURL(video, 1)); code != http.StatusOK || !bytes.Equal(seg, o.fixture(t, "cenc", video, "seg-1.m4s")) {
			t.Errorf("video segment 1: %d, want 200 with the origin's ciphertext", code)
		}
	}

	postKey(t, mgr, cencVideoKey)
	waitFor(t, "missing_key for the rotation KID only", onlyRotationMissing)
	stillCiphertext()

	postKey(t, mgr, cencUnusedKey)
	time.Sleep(500 * time.Millisecond)
	if !onlyRotationMissing() {
		t.Errorf("after an unrelated key: warnings %+v, want only missing_key for %s", decryptWarnings(t, mgr), rotKID)
	}
	stillCiphertext()

	postKey(t, mgr, cencRotationKey)
	waitFor(t, "the video AdaptationSet to be converted", func() bool {
		code, mpd := get(t, h, "/v1/channels/c/manifest.mpd")
		return code == http.StatusOK && !bytes.Contains(mpd, []byte("ContentProtection"))
	})
	for n := 1; n <= 4; n++ {
		code, seg := get(t, h, segmentURL(video, n)+"?c=1")
		if code != http.StatusOK || !bytes.Equal(mdatOf(t, seg), mdatOf(t, o.fixture(t, "clear", video, fmt.Sprintf("seg-%d.m4s", n)))) {
			t.Errorf("converted video segment %d: %d, want 200 with the clear samples", n, code)
		}
	}
	for _, w := range decryptWarnings(t, mgr) {
		if w.Kind != dashdecrypt.KindKeyUnmatched {
			t.Errorf("warning %+v left after the conversion", w)
		}
	}
}
