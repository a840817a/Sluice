package channel

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/a840817a/sluice/internal/cenc"
	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/presentation"
	"github.com/a840817a/sluice/internal/queue"
)

func parseFixtureMPD(t *testing.T) *imdp.ParsedMPD {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/cenc/cenc/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	p, err := imdp.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func cpCount(as *imdp.ParsedAdaptationSet) int {
	return len(as.ContentProtections) + len(as.RawCPs) + len(as.ParsedContentProtections)
}

// decryptAS has reg decrypt the fixture AdaptationSet asID of the period with
// no id, as ingest would on its first init.
func decryptAS(t *testing.T, reg *dashdecrypt.Registry, asID, mediaType, initPath string) {
	t.Helper()
	init, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatal(err)
	}
	task := &queue.SegmentTask{ChannelID: "c", ASID: asID, RepID: "r", MediaType: mediaType}
	if _, err := reg.PrepareInit(context.Background(), task, init, false, nil); err != nil {
		t.Fatal(err)
	}
	if !reg.IsDecrypted(dashdecrypt.ASKey{ASID: asID}) {
		t.Fatalf("AdaptationSet %s not decrypted", asID)
	}
}

// ParsedMPD strips the ContentProtection of exactly the AdaptationSets the
// registry decrypted, never touches the MPD it was given, and follows the
// registry as more are decrypted.
func TestParsedMPDStripsDecryptedContentProtection(t *testing.T) {
	keys := cenc.KeySet{}
	for _, s := range []string{
		"10101010101010101010101010101010:00112233445566778899aabbccddeeff",
		"20202020202020202020202020202020:ffeeddccbbaa99887766554433221100",
	} {
		k, err := cenc.ParseKey(s)
		if err != nil {
			t.Fatal(err)
		}
		keys[k.KID] = k.Value
	}
	reg := dashdecrypt.NewRegistry(t.TempDir(), "c", keys)
	rt := &Runtime{decrypt: reg}
	p := parseFixtureMPD(t)
	video, audio := p.Periods[0].AdaptationSets[0], p.Periods[0].AdaptationSets[1]
	if cpCount(video) == 0 || cpCount(audio) == 0 {
		t.Fatal("fixture AdaptationSets carry no ContentProtection; this test proves nothing")
	}

	if got := rt.withoutDecryptedCP(p); got != p {
		t.Error("nothing decrypted, yet the MPD was copied")
	}
	if presentation.TracksFromMPD(rt.withoutDecryptedCP(p)).ServesHLS {
		t.Error("HLS served before anything was decrypted")
	}

	decryptAS(t, reg, "0", "video", "../../testdata/cenc/cenc/video/avc1/init.mp4")
	got := rt.withoutDecryptedCP(p)
	if got == p {
		t.Fatal("the cached view survived a generation change")
	}
	if n := cpCount(got.Periods[0].AdaptationSets[0]); n != 0 {
		t.Errorf("decrypted video AdaptationSet still has %d ContentProtection entries", n)
	}
	if got.Periods[0].AdaptationSets[1] != audio {
		t.Error("the encrypted audio AdaptationSet was changed")
	}
	if cpCount(video) == 0 {
		t.Error("the source MPD was modified")
	}
	if rt.withoutDecryptedCP(p) != got {
		t.Error("an unchanged generation rebuilt the view")
	}
	if presentation.TracksFromMPD(got).ServesHLS {
		t.Error("HLS served while the audio AdaptationSet is encrypted")
	}

	decryptAS(t, reg, "1", "audio", "../../testdata/cenc/cenc/audio/und/mp4a.40.2/init.mp4")
	all := rt.withoutDecryptedCP(p)
	if n := cpCount(all.Periods[0].AdaptationSets[1]); n != 0 {
		t.Errorf("decrypted audio AdaptationSet still has %d ContentProtection entries", n)
	}
	if !presentation.TracksFromMPD(all).ServesHLS {
		t.Error("HLS not served once every AdaptationSet is clear")
	}
	if ws := rt.decryptWarnings(all, true); len(ws) != 0 {
		t.Errorf("warnings = %+v, want none", ws)
	}
}

// A channel without a registry gets the MPD as it is.
func TestParsedMPDUnchangedWithoutRegistry(t *testing.T) {
	p := parseFixtureMPD(t)
	if got := (&Runtime{}).withoutDecryptedCP(p); got != p {
		t.Error("a passthrough channel's MPD was copied")
	}
}

// A channel whose interrupted conversion cannot be recovered without losing
// a copy of an AdaptationSet does not start: ingest would write into the
// live directory and the copy moved aside would then be at risk.
func TestStartFailsWhenStagingUnrecoverable(t *testing.T) {
	dataDir := t.TempDir()
	channelDir := filepath.Join(dataDir, "c")
	for _, dir := range []string{
		filepath.Join(channelDir, "periods", "p0", "audio_1", "a0"),
		filepath.Join(channelDir, dashdecrypt.StagingDirName, "p0", "audio_1.old", "a0"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestManager(t, gocfg.Config{Store: gocfg.StoreConfig{DataDir: dataDir}})
	rt, err := m.startForTest(context.Background(), "c", Config{
		MPDURL: "http://127.0.0.1:1/manifest.mpd", KeyMode: KeyModeDecrypt,
		ContentKeys: []string{"10101010101010101010101010101010:00112233445566778899aabbccddeeff"}, Enabled: true,
	})
	if err == nil || rt != nil || m.Get("c") != nil {
		m.StopAll()
		t.Fatalf("start = %v, %v; want an error and no running channel", rt, err)
	}
	if _, err := os.Stat(filepath.Join(channelDir, dashdecrypt.StagingDirName, "p0", "audio_1.old")); err != nil {
		t.Errorf("the copy moved aside is gone: %v", err)
	}
}
