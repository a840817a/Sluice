package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/store"
)

// Converting an AdaptationSet a DASH decrypt channel stored as ciphertext,
// once the key it lacked is added.

// cencAudioLoc is where channel "c" stores the fixtures' audio
// representation.
func cencAudioLoc(dataDir string) store.RepLocation {
	return store.RepLocation{DataDir: dataDir, ChannelID: "c", MediaType: "audio", ASID: "1", RepID: "a"}
}

// waitFor polls cond until it holds, and fails the test after 20 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitConverted waits for the audio AdaptationSet to be swapped in clear:
// its decrypt.json is on disk and the served manifest no longer protects it.
func waitConverted(t *testing.T, h http.Handler, dataDir string) {
	t.Helper()
	state := filepath.Join(cencAudioLoc(dataDir).ASDir(), dashdecrypt.StateFileName)
	waitFor(t, "the audio AdaptationSet to be converted", func() bool {
		if _, err := os.Stat(state); err != nil {
			return false
		}
		code, mpd := get(t, h, "/v1/channels/c/manifest.mpd")
		return code == http.StatusOK && !bytes.Contains(mpd, []byte("ContentProtection"))
	})
}

// addAudioKey adds the audio key through the admin API.
func addAudioKey(t *testing.T, mgr *ch.Manager) {
	t.Helper()
	w := postKeys(NewAdminServer(context.Background(), mgr), "c", map[string][]string{"content_keys": {cencAudioKey}})
	if w.Code != http.StatusOK {
		t.Fatalf("POST keys: %d %s", w.Code, w.Body)
	}
}

// assertConvertedOutput checks what a channel serves once its audio
// AdaptationSet is converted: clear audio under versioned URLs, a manifest
// without ContentProtection, HLS with every segment, and no warning left.
func assertConvertedOutput(t *testing.T, mgr *ch.Manager, h http.Handler, o *cencOrigin) {
	t.Helper()
	video, audio := cencReps[0], cencReps[1]
	for _, rep := range cencReps {
		assertDecrypted(t, h, o, rep, rep.segments)
	}

	code, body := get(t, h, "/v1/channels/c/manifest.mpd")
	mpd := string(body)
	if code != http.StatusOK || strings.Contains(mpd, "ContentProtection") {
		t.Errorf("manifest: %d, want 200 without ContentProtection:\n%s", code, mpd)
	}
	videoAS, audioAS, _ := strings.Cut(mpd, `mimeType="audio/mp4"`)
	if !strings.Contains(audioAS, `/a/$Number%09d$.m4s?c=1"`) || !strings.Contains(audioAS, `/a.mp4?c=1"`) {
		t.Errorf("converted audio URLs carry no version:\n%s", mpd)
	}
	if strings.Contains(videoAS, "?c=") {
		t.Errorf("video, decrypted from its first file, has versioned URLs:\n%s", mpd)
	}

	if code, master := get(t, h, "/v1/channels/c/master.m3u8"); code != http.StatusOK {
		t.Fatalf("master: %d, want 200 once every AdaptationSet is clear:\n%s", code, master)
	}
	for _, rep := range cencReps {
		code, pl := get(t, h, "/v1/channels/c/media/"+rep.id+".m3u8")
		// A live channel also publishes the audio's short fifth segment.
		n := strings.Count(string(pl), "#EXTINF:")
		if code != http.StatusOK || n < rep.segments {
			t.Errorf("%s playlist: %d with %d segments, want 200 with at least %d:\n%s", rep.id, code, n, rep.segments, pl)
		}
		versioned := strings.Count(string(pl), "?c=1")
		if want := map[string]int{video.id: 0, audio.id: n + 1}[rep.id]; versioned != want {
			t.Errorf("%s playlist has %d versioned URLs, want %d:\n%s", rep.id, versioned, want, pl)
		}
	}
	for i := 1; i <= audio.segments; i++ {
		if code, seg := get(t, h, segmentURL(audio, i)+"?c=1"); code != http.StatusOK ||
			!bytes.Equal(mdatOf(t, seg), mdatOf(t, o.fixture(t, "clear", audio, fmt.Sprintf("seg-%d.m4s", i)))) {
			t.Errorf("versioned audio segment %d: %d, want 200 with clear samples", i, code)
		}
	}

	if ws := decryptWarnings(t, mgr); len(ws) != 0 {
		t.Errorf("decrypt warnings after conversion = %+v, want none", ws)
	}
}

func assertNoStaging(t *testing.T, dataDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(store.ChannelDir(dataDir, "c"), dashdecrypt.StagingDirName)); !os.IsNotExist(err) {
		t.Errorf("staging left behind: %v", err)
	}
}

// fetchRaw requests path without a testing.T, for use off the test goroutine.
func fetchRaw(h http.Handler, path string) (int, []byte) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, body
}

// A live channel created with the video key only stores audio as
// ciphertext. Once the audio key is added, the stored audio is converted:
// the channel serves it clear under new URLs, signals no protection, opens
// HLS, and drops its warnings. Readers during the swap see a 404, the
// ciphertext or the clear segment, never anything else.
func TestDASHConvertLiveAfterKeysAdded(t *testing.T) {
	o := startCENCOrigin(t, "cenc", liveCENC(false))
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey)
	audio := cencReps[1]
	for i := 1; i <= audio.segments; i++ {
		waitFor(t, fmt.Sprintf("audio segment %d", i), func() bool {
			code, _ := get(t, h, segmentURL(audio, i))
			return code == http.StatusOK
		})
	}
	if code, seg := get(t, h, segmentURL(audio, 1)); code != http.StatusOK || !bytes.Equal(seg, o.fixture(t, "cenc", audio, "seg-1.m4s")) {
		t.Fatalf("audio before the key: %d, want the origin's ciphertext", code)
	}
	if code, _ := get(t, h, "/v1/channels/c/master.m3u8"); code == http.StatusOK {
		t.Fatal("HLS served before the audio key was added")
	}

	cipher := make(map[int][]byte)
	clear := make(map[int][]byte)
	for i := 1; i <= audio.segments; i++ {
		cipher[i] = o.fixture(t, "cenc", audio, fmt.Sprintf("seg-%d.m4s", i))
		clear[i] = mdatOf(t, o.fixture(t, "clear", audio, fmt.Sprintf("seg-%d.m4s", i)))
	}
	var bad atomic.Value
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			for i := 1; i <= audio.segments; i++ {
				select {
				case <-stop:
					return
				default:
				}
				for _, u := range []string{segmentURL(audio, i), segmentURL(audio, i) + "?c=1"} {
					code, seg := fetchRaw(h, u)
					if code == http.StatusNotFound || bytes.Equal(seg, cipher[i]) {
						continue
					}
					if code != http.StatusOK || len(seg) != len(cipher[i]) || !bytes.Contains(seg, clear[i]) {
						bad.Store(fmt.Sprintf("%s: %d with %d bytes, neither ciphertext nor clear", u, code, len(seg)))
					}
				}
			}
		}
	}()

	addAudioKey(t, mgr)
	waitConverted(t, h, dataDir)
	close(stop)
	readers.Wait()
	if msg := bad.Load(); msg != nil {
		t.Errorf("read during the swap: %s", msg)
	}
	if rt := mgr.Get("c"); rt == nil || rt.Mode() != ch.ModeLive {
		t.Fatal("channel is no longer live")
	}
	assertConvertedOutput(t, mgr, h, o)
	assertNoStaging(t, dataDir)
}

// A channel that finished as VOD with its audio encrypted converts it when
// the key is added: the served manifest and the saved one lose the audio's
// ContentProtection, and HLS opens.
func TestDASHConvertVODAfterKeysAdded(t *testing.T) {
	o := startCENCOrigin(t, "cenc", nil)
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey)
	waitVOD(t, mgr)
	saved := filepath.Join(store.ChannelDir(dataDir, "c"), "manifest.mpd")
	if b, err := os.ReadFile(saved); err != nil || !bytes.Contains(b, []byte("ContentProtection")) {
		t.Fatalf("saved manifest before the key: %v, want one protecting the audio", err)
	}
	if code, mpd := get(t, h, "/v1/channels/c/manifest.mpd"); code != http.StatusOK || !bytes.Contains(mpd, []byte("ContentProtection")) {
		t.Fatalf("served manifest before the key: %d, want one protecting the audio", code)
	}
	if code, _ := get(t, h, "/v1/channels/c/master.m3u8"); code == http.StatusOK {
		t.Fatal("HLS served before the audio key was added")
	}

	addAudioKey(t, mgr)
	waitConverted(t, h, dataDir)
	assertConvertedOutput(t, mgr, h, o)
	assertNoStaging(t, dataDir)
	b, err := os.ReadFile(saved)
	if err != nil || bytes.Contains(b, []byte("ContentProtection")) || !bytes.Contains(b, []byte("a.mp4?c=1")) {
		t.Errorf("saved manifest after conversion (%v) still protects the audio or names its old URLs:\n%s", err, b)
	}
}

// Keys added while the channel is stopped are stored only; the channel
// converts what they complete when it starts again.
func TestDASHConvertKeysAddedWhileStopped(t *testing.T) {
	o := startCENCOrigin(t, "cenc", nil)
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey)
	waitVOD(t, mgr)
	if err := mgr.Stop("c"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddContentKeys(context.Background(), "c", []string{cencAudioKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cencAudioLoc(dataDir).ASDir(), dashdecrypt.StateFileName)); !os.IsNotExist(err) {
		t.Fatalf("stopped channel converted: %v", err)
	}

	if err := mgr.Restart(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	waitConverted(t, h, dataDir)
	assertConvertedOutput(t, mgr, h, o)
	assertNoStaging(t, dataDir)
	saved := filepath.Join(store.ChannelDir(dataDir, "c"), "manifest.mpd")
	if b, err := os.ReadFile(saved); err != nil || bytes.Contains(b, []byte("ContentProtection")) {
		t.Errorf("saved manifest after conversion (%v) still protects the audio", err)
	}
}

// A conversion that fails leaves the audio exactly as stored and served,
// protected in the manifest, and says so in a conversion_failed warning.
func TestDASHConvertFailureKeepsCiphertext(t *testing.T) {
	o := startCENCOrigin(t, "cenc", nil)
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey)
	waitVOD(t, mgr)
	audio := cencReps[1]
	corrupt := o.fixture(t, "cenc", audio, "seg-2.m4s")[:100]
	if err := os.WriteFile(cencAudioLoc(dataDir).SegmentPath(2, store.ExtFMP4), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	addAudioKey(t, mgr)
	waitFor(t, "conversion_failed", func() bool {
		for _, w := range decryptWarnings(t, mgr) {
			if w.Kind == dashdecrypt.KindConversionFailed && w.AdaptationSet == "1" {
				return true
			}
		}
		return false
	})

	for i := 1; i <= audio.segments; i++ {
		want := o.fixture(t, "cenc", audio, fmt.Sprintf("seg-%d.m4s", i))
		if i == 2 {
			want = corrupt
		}
		if code, seg := get(t, h, segmentURL(audio, i)); code != http.StatusOK || !bytes.Equal(seg, want) {
			t.Errorf("audio segment %d: %d, want 200 with the bytes stored before", i, code)
		}
	}
	if code, init := get(t, h, initURL(audio)); code != http.StatusOK || !bytes.Equal(init, o.fixture(t, "cenc", audio, "init.mp4")) {
		t.Errorf("audio init: %d, want the stored ciphertext init", code)
	}
	if _, mpd := get(t, h, "/v1/channels/c/manifest.mpd"); !bytes.Contains(mpd, []byte("ContentProtection")) || bytes.Contains(mpd, []byte("?c=")) {
		t.Errorf("manifest after a failed conversion lost the audio's protection or versioned its URLs:\n%s", mpd)
	}
	if code, _ := get(t, h, "/v1/channels/c/master.m3u8"); code == http.StatusOK {
		t.Error("HLS served with the audio still encrypted")
	}
	assertNoStaging(t, dataDir)
}

// A VOD restored from its saved manifest alone, without the origin MPD it
// was rendered from, cannot describe converted media: the manifest would go
// on protecting it under its old URLs. So its media is left as stored and a
// warning says why.
func TestDASHConvertSkipsVODWithOnlySavedManifest(t *testing.T) {
	o := startCENCOrigin(t, "cenc", nil)
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey)
	waitVOD(t, mgr)
	if err := mgr.Stop("c"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.ChannelDir(dataDir, "c"), "upstream.mpd")); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddContentKeys(context.Background(), "c", []string{cencAudioKey}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Restart(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	_, before := get(t, h, "/v1/channels/c/manifest.mpd")

	waitFor(t, "a warning that the audio cannot be converted", func() bool {
		for _, w := range decryptWarnings(t, mgr) {
			if w.Kind == dashdecrypt.KindConversionFailed && w.AdaptationSet == "1" {
				return !strings.Contains(w.Message, keyValue(cencAudioKey))
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(cencAudioLoc(dataDir).ASDir(), dashdecrypt.StateFileName)); !os.IsNotExist(err) {
		t.Errorf("audio converted although the manifest cannot describe it: %v", err)
	}
	audio := cencReps[1]
	if code, seg := get(t, h, segmentURL(audio, 1)); code != http.StatusOK || !bytes.Equal(seg, o.fixture(t, "cenc", audio, "seg-1.m4s")) {
		t.Errorf("audio segment 1: %d, want the stored ciphertext", code)
	}
	if _, after := get(t, h, "/v1/channels/c/manifest.mpd"); !bytes.Equal(after, before) || !bytes.Contains(after, []byte("ContentProtection")) {
		t.Errorf("saved manifest changed or lost its protection:\n%s", after)
	}
	assertNoStaging(t, dataDir)
}
