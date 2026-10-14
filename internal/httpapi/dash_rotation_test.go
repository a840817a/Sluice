package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/cenc/cenctest"
	"github.com/a840817a/sluice/internal/dashdecrypt"
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
