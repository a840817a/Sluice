package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/dashdecrypt"
)

// The joining representation "w" is the video under another KID: its init
// is the video's with the tenc KID rewritten to joinKID, and its
// segments are the video's, so joinKey, the video key's value under
// joinKID, decrypts them.
const (
	joinKID = "30303030303030303030303030303030"
	joinKey = joinKID + ":00112233445566778899aabbccddeeff"
)

var joinRep = cencRep{id: "w", dir: "video/avc1", segments: 4}

// serveJoiningRep has o serve w's init and segments, and lists w in the
// video AdaptationSet of the live MPD from now on.
func serveJoiningRep(t *testing.T, o *cencOrigin) {
	t.Helper()
	video := cencReps[0]
	init := o.fixture(t, "cenc", video, "init.mp4")
	// tenc: type, version and flags, two reserved bytes, isProtected, the
	// per-sample IV size, then the KID.
	kid := bytes.Index(init, []byte("tenc")) + 4 + 4 + 4
	if kid < 12 || !bytes.Equal(init[kid:kid+16], bytes.Repeat([]byte{0x10}, 16)) {
		t.Fatal("video init's tenc carries no video KID to rewrite")
	}
	copy(init[kid:], bytes.Repeat([]byte{0x30}, 16))
	o.extra.Store("w/init.mp4", init)
	for i := 1; i <= joinRep.segments; i++ {
		file := fmt.Sprintf("seg-%d.m4s", i)
		o.extra.Store("w/"+file, o.fixture(t, "cenc", video, file))
	}
	live := liveCENC(false)
	o.setEdit(func(mpd string) string {
		mpd = live(mpd)
		rep := `<Representation id="v" codecs="avc1.42D00B" width="160" height="90" scanType="progressive" frameRate="24" bandwidth="43120"/>`
		if !strings.Contains(mpd, rep) {
			panic("video Representation not found in the fixture MPD")
		}
		return strings.Replace(mpd, rep, rep+"\n      "+strings.Replace(strings.Replace(rep, `id="v"`, `id="w"`, 1),
			`bandwidth="43120"`, `bandwidth="40000"`, 1), 1)
	})
}

func hasMissingJoinKey(ws []dashdecrypt.Warning) bool {
	return slices.ContainsFunc(ws, func(w dashdecrypt.Warning) bool {
		return w.Kind == dashdecrypt.KindMissingKey && w.AdaptationSet == "0" && slices.Contains(w.KIDs, joinKID)
	})
}

// A Representation that joins an AdaptationSet the channel already decrypts,
// under a KID it holds no key for, is refused: the manifest and the HLS
// master leave it out while the rest still plays, and a missing_key warning
// names its KID. Once the key is added, a live channel stores its init and
// the segments the origin already listed, without the origin adding any,
// and serves it decrypted; the warning goes.
func TestDASHDecryptJoiningRepWaitsForItsKey(t *testing.T) {
	o := startCENCOrigin(t, "cenc", liveCENC(false))
	mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey, cencAudioKey)
	for _, rep := range cencReps {
		waitFor(t, rep.id+" segment 1", func() bool {
			code, _ := get(t, h, segmentURL(rep, 1))
			return code == http.StatusOK
		})
	}

	serveJoiningRep(t, o)
	waitFor(t, "the missing_key warning for w's KID", func() bool { return hasMissingJoinKey(decryptWarnings(t, mgr)) })
	code, mpd := get(t, h, "/v1/channels/c/manifest.mpd")
	if code != http.StatusOK || bytes.Contains(mpd, []byte(`id="w"`)) || !bytes.Contains(mpd, []byte(`id="v"`)) {
		t.Errorf("manifest with w refused: %d, want 200 listing v but not w:\n%s", code, mpd)
	}
	code, master := get(t, h, "/v1/channels/c/master.m3u8")
	if code != http.StatusOK || bytes.Contains(master, []byte("w.m3u8")) || !bytes.Contains(master, []byte("v.m3u8")) {
		t.Errorf("master with w refused: %d, want 200 listing v but not w:\n%s", code, master)
	}
	for _, rep := range cencReps {
		assertDecrypted(t, h, o, rep, rep.segments)
	}
	if code, _ := get(t, h, initURL(joinRep)); code != http.StatusNotFound {
		t.Errorf("refused w's init: %d, want 404", code)
	}

	// The key arrives only after w's init preparation has given up and its
	// segments were dropped, so nothing in flight can pick it up.
	waitFor(t, "w's segments to be dropped", func() bool {
		info, _ := mgr.Channel("c")
		p := info.Status.Progress
		return p != nil && p.LastSegmentError != nil && p.LastSegmentError.RepID == joinRep.id &&
			strings.Contains(p.LastSegmentError.Message, "init unavailable")
	})
	if w := postKeys(NewAdminServer(t.Context(), mgr), "c", map[string][]string{"content_keys": {joinKey}}); w.Code != http.StatusOK {
		t.Fatalf("POST keys: %d %s", w.Code, w.Body)
	}
	waitFor(t, "w's init and segments", func() bool {
		if code, _ := get(t, h, initURL(joinRep)); code != http.StatusOK {
			return false
		}
		for i := 1; i <= joinRep.segments; i++ {
			if code, _ := get(t, h, segmentURL(joinRep, i)); code != http.StatusOK {
				return false
			}
		}
		return true
	})
	assertDecrypted(t, h, o, joinRep, joinRep.segments)
	waitFor(t, "w in the manifest and the master playlist", func() bool {
		_, mpd := get(t, h, "/v1/channels/c/manifest.mpd")
		_, master := get(t, h, "/v1/channels/c/master.m3u8")
		return bytes.Contains(mpd, []byte(`id="w"`)) && bytes.Contains(master, []byte("w.m3u8"))
	})
	if ws := decryptWarnings(t, mgr); len(ws) != 0 {
		t.Errorf("decrypt warnings once w is stored = %+v, want none", ws)
	}
}
