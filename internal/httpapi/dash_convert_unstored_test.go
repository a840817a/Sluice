package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/store"
)

// liveCENCWithJoinRep is liveCENC with joinRep "w" listed in the video
// AdaptationSet beside v.
func liveCENCWithJoinRep(firstTwo bool) func(string) string {
	live := liveCENC(firstTwo)
	return func(mpd string) string {
		mpd = live(mpd)
		rep := `<Representation id="v" codecs="avc1.42D00B" width="160" height="90" scanType="progressive" frameRate="24" bandwidth="43120"/>`
		if !strings.Contains(mpd, rep) {
			panic("video Representation not found in the fixture MPD")
		}
		return strings.Replace(mpd, rep, rep+"\n      "+strings.Replace(strings.Replace(rep, `id="v"`, `id="w"`, 1),
			`bandwidth="43120"`, `bandwidth="40000"`, 1), 1)
	}
}

// A stored ciphertext AdaptationSet is converted only once every
// representation its MPD lists has a stored init with a key held. Here w's
// init could not be fetched, so the video AdaptationSet is stored as
// ciphertext and w has nothing on disk: adding a key must not convert it
// from v alone, which would refuse, and hide, w once its init arrives. Once
// w's init is stored and its key added, the whole AdaptationSet converts.
func TestDASHConvertWaitsForEveryListedRepresentation(t *testing.T) {
	o := startCENCOrigin(t, "cenc", liveCENCWithJoinRep(true))
	video := cencReps[0]
	init := o.fixture(t, "cenc", video, "init.mp4")
	kid := bytes.Index(init, []byte("tenc")) + 4 + 4 + 4
	if kid < 12 || !bytes.Equal(init[kid:kid+16], bytes.Repeat([]byte{0x10}, 16)) {
		t.Fatal("video init's tenc carries no video KID to rewrite")
	}
	copy(init[kid:], bytes.Repeat([]byte{0x30}, 16))
	for i := 1; i <= joinRep.segments; i++ {
		file := fmt.Sprintf("seg-%d.m4s", i)
		o.extra.Store("w/"+file, o.fixture(t, "cenc", video, file))
	}

	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey, cencAudioKey)
	videoLoc := store.RepLocation{DataDir: dataDir, ChannelID: "c", MediaType: "video", ASID: "0", RepID: "v"}
	state := filepath.Join(videoLoc.ASDir(), dashdecrypt.StateFileName)
	waitFor(t, "w's segments to be dropped for want of its init", func() bool {
		info, _ := mgr.Channel("c")
		p := info.Status.Progress
		return p != nil && p.LastSegmentError != nil && p.LastSegmentError.RepID == joinRep.id &&
			strings.Contains(p.LastSegmentError.Message, "init unavailable")
	})
	waitFor(t, "v segment 2", func() bool {
		code, _ := get(t, h, segmentURL(video, 2))
		return code == http.StatusOK
	})
	if code, seg := get(t, h, segmentURL(video, 1)); code != http.StatusOK || !bytes.Equal(seg, o.fixture(t, "cenc", video, "seg-1.m4s")) {
		t.Fatalf("v segment 1: %d, want the origin's ciphertext", code)
	}

	const unrelatedKey = "50505050505050505050505050505050:505152535455565758595a5b5c5d5e5f"
	if w := postKeys(NewAdminServer(context.Background(), mgr), "c", map[string][]string{"content_keys": {unrelatedKey}}); w.Code != http.StatusOK {
		t.Fatalf("POST keys: %d %s", w.Code, w.Body)
	}
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(state); err == nil {
			t.Fatal("video AdaptationSet converted while w, which the MPD lists, has no stored init")
		}
	}

	// w's init becomes available, and the origin lists more segments, so
	// the channel stores it as ciphertext; then w's key arrives.
	o.extra.Store("w/init.mp4", init)
	o.setEdit(liveCENCWithJoinRep(false))
	waitFor(t, "w segment 3", func() bool {
		code, _ := get(t, h, segmentURL(joinRep, 3))
		return code == http.StatusOK
	})
	if _, err := os.Stat(state); err == nil {
		t.Fatal("video AdaptationSet converted before w's key was held")
	}
	if w := postKeys(NewAdminServer(context.Background(), mgr), "c", map[string][]string{"content_keys": {joinKey}}); w.Code != http.StatusOK {
		t.Fatalf("POST keys: %d %s", w.Code, w.Body)
	}
	waitFor(t, "the video AdaptationSet to be converted", func() bool {
		if _, err := os.Stat(state); err != nil {
			return false
		}
		code, mpd := get(t, h, "/v1/channels/c/manifest.mpd")
		videoAS, _, _ := strings.Cut(string(mpd), `mimeType="audio/mp4"`)
		return code == http.StatusOK && !strings.Contains(videoAS, "ContentProtection") &&
			strings.Contains(videoAS, `id="v"`) && strings.Contains(videoAS, `id="w"`)
	})
	waitFor(t, "v segment 4", func() bool {
		code, _ := get(t, h, segmentURL(video, 4))
		return code == http.StatusOK
	})
	assertDecrypted(t, h, o, video, video.segments)
	code, wInit := get(t, h, initURL(joinRep)+"?c=1")
	if code != http.StatusOK || bytes.Contains(wInit, []byte("tenc")) {
		t.Errorf("w init after conversion: %d, want 200 and clear", code)
	}
	code, seg := get(t, h, segmentURL(joinRep, 3))
	if code != http.StatusOK || !bytes.Equal(mdatOf(t, seg), mdatOf(t, o.fixture(t, "clear", video, "seg-3.m4s"))) {
		t.Errorf("w segment 3 after conversion: %d, want the clear fixture's samples", code)
	}
}

// A live channel restarted with the key its stored ciphertext lacked
// converts it once the restarted watcher has read the MPD: before that, the
// representations the AdaptationSet must cover are unknown.
func TestDASHConvertLiveRestartWaitsForTheMPD(t *testing.T) {
	o := startCENCOrigin(t, "cenc", liveCENC(false))
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey)
	audio := cencReps[1]
	waitFor(t, "audio segment 4", func() bool {
		code, _ := get(t, h, segmentURL(audio, 4))
		return code == http.StatusOK
	})
	if err := mgr.Stop("c"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddContentKeys(context.Background(), "c", []string{cencAudioKey}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Restart(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	waitConverted(t, h, dataDir)
	assertDecrypted(t, h, o, audio, audio.segments)
}
