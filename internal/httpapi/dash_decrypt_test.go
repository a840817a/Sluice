package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/dashdecrypt"
	"github.com/a840817a/sluice/internal/store"
)

// The fixtures in testdata/cenc are one clip packaged clear, with cenc and
// with cbcs (see its README); video and audio have different keys.
const (
	cencFixtures = "../../testdata/cenc"
	cencVideoKey = "10101010101010101010101010101010:00112233445566778899aabbccddeeff"
	cencAudioKey = "20202020202020202020202020202020:ffeeddccbbaa99887766554433221100"
	// cencUnusedKey is a key no fixture init uses.
	cencUnusedKey = "30303030303030303030303030303030:0123456789abcdef0123456789abcdef"
	// cencPeriod is the URL segment of the fixtures' Period, which has no id.
	cencPeriod = "~"
)

// cencRep is one representation of the fixtures, under the short ID the test
// origin gives it so the gateway's URLs need no encoding.
type cencRep struct {
	id, dir  string
	segments int
}

// segments counts the published ones: the audio's fifth, 16 ms past the end
// of the video, never passes the A/V barrier.
var cencReps = []cencRep{
	{id: "v", dir: "video/avc1", segments: 4},
	{id: "a", dir: "audio/und/mp4a.40.2", segments: 4},
}

// cencOrigin serves the fixtures of one variant as a DASH origin. Its MPD is
// the variant's manifest.mpd with the representation IDs shortened and with
// edit applied; edit can change between requests.
type cencOrigin struct {
	srv     *httptest.Server
	variant string
	edit    atomic.Pointer[func(string) string]
}

func startCENCOrigin(t *testing.T, variant string, edit func(string) string) *cencOrigin {
	t.Helper()
	o := &cencOrigin{variant: variant}
	o.setEdit(edit)
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(filepath.Join(cencFixtures, variant, "manifest.mpd"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mpd := string(raw)
		for _, rep := range cencReps {
			mpd = strings.Replace(mpd, `id="`+rep.dir+`"`, `id="`+rep.id+`"`, 1)
		}
		if e := o.edit.Load(); e != nil && *e != nil {
			mpd = (*e)(mpd)
		}
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, mpd)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		repID, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		for _, rep := range cencReps {
			if ok && rep.id == repID {
				http.ServeFile(w, r, filepath.Join(cencFixtures, variant, rep.dir, file))
				return
			}
		}
		http.NotFound(w, r)
	})
	o.srv = httptest.NewServer(mux)
	t.Cleanup(o.srv.Close)
	return o
}

func (o *cencOrigin) setEdit(edit func(string) string) { o.edit.Store(&edit) }

func (o *cencOrigin) fixture(t *testing.T, variant string, rep cencRep, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cencFixtures, variant, rep.dir, file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mdatOf returns the payload of the top-level mdat box of an fMP4 segment.
func mdatOf(t *testing.T, seg []byte) []byte {
	t.Helper()
	for off := 0; off+8 <= len(seg); {
		size := int(binary.BigEndian.Uint32(seg[off:]))
		if size < 8 || off+size > len(seg) {
			break
		}
		if string(seg[off+4:off+8]) == "mdat" {
			return seg[off+8 : off+size]
		}
		off += size
	}
	t.Fatalf("segment of %d bytes has no mdat", len(seg))
	return nil
}

// newCENCChannel starts DASH decrypt channel "c" on origin with keys and
// returns its manager and a router serving it.
func newCENCChannel(t *testing.T, o *cencOrigin, dataDir string, keys ...string) (*ch.Manager, http.Handler) {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dataDir},
		Worker: config.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := newTestManager(t, cfg)
	if _, err := createChannel(mgr, context.Background(), "c", ch.Config{
		MPDURL:      o.srv.URL + "/manifest.mpd",
		KeyMode:     ch.KeyModeDecrypt,
		ContentKeys: keys,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(mgr.StopAll)
	return mgr, periodRouter(t, cfg, mgr)
}

func waitVOD(t *testing.T, mgr *ch.Manager) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if rt := mgr.Get("c"); rt != nil && rt.Mode() == ch.ModeVOD {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("channel never reached VOD")
}

func segmentURL(rep cencRep, n int) string {
	return fmt.Sprintf("/v1/channels/c/segments/%s/%s/%d.m4s", cencPeriod, rep.id, n)
}

func initURL(rep cencRep) string {
	return fmt.Sprintf("/v1/channels/c/init/%s/%s.mp4", cencPeriod, rep.id)
}

// assertDecrypted checks that rep's served init carries no protection and
// that each of its first n served segments holds the clear fixture's samples.
func assertDecrypted(t *testing.T, h http.Handler, o *cencOrigin, rep cencRep, n int) {
	t.Helper()
	code, init := get(t, h, initURL(rep))
	if code != http.StatusOK {
		t.Fatalf("%s init: %d", rep.id, code)
	}
	for _, box := range []string{"encv", "enca", "sinf", "pssh", "tenc"} {
		if bytes.Contains(init, []byte(box)) {
			t.Errorf("%s served init still has a %s box", rep.id, box)
		}
	}
	for i := 1; i <= n; i++ {
		code, seg := get(t, h, segmentURL(rep, i))
		if code != http.StatusOK {
			t.Errorf("%s segment %d: %d", rep.id, i, code)
			continue
		}
		clear := o.fixture(t, "clear", rep, fmt.Sprintf("seg-%d.m4s", i))
		if !bytes.Equal(mdatOf(t, seg), mdatOf(t, clear)) {
			t.Errorf("%s segment %d: served samples differ from the clear fixture's", rep.id, i)
		}
		if len(seg) != len(o.fixture(t, o.variant, rep, fmt.Sprintf("seg-%d.m4s", i))) {
			t.Errorf("%s segment %d: length changed by decryption", rep.id, i)
		}
	}
}

func decryptWarnings(t *testing.T, mgr *ch.Manager) []dashdecrypt.Warning {
	t.Helper()
	info, ok := mgr.Channel("c")
	if !ok {
		t.Fatal("channel c missing")
	}
	return info.Status.DecryptWarnings
}

// A DASH decrypt channel holding every key stores both AdaptationSets clear:
// the served segments carry the clear samples, the inits and the manifest
// signal no protection, and the channel is served as HLS too.
func TestDASHDecryptAllKeysServesClear(t *testing.T) {
	for _, variant := range []string{"cenc", "cbcs"} {
		t.Run(variant, func(t *testing.T) {
			o := startCENCOrigin(t, variant, nil)
			mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey, cencAudioKey, cencUnusedKey)
			waitVOD(t, mgr)

			for _, rep := range cencReps {
				assertDecrypted(t, h, o, rep, rep.segments)
			}
			code, mpd := get(t, h, "/v1/channels/c/manifest.mpd")
			if code != http.StatusOK || bytes.Contains(mpd, []byte("ContentProtection")) {
				t.Errorf("manifest: %d, want 200 without ContentProtection:\n%s", code, mpd)
			}

			code, master := get(t, h, "/v1/channels/c/master.m3u8")
			if code != http.StatusOK || !bytes.Contains(master, []byte("/media/v.m3u8")) || !bytes.Contains(master, []byte("/media/a.m3u8")) {
				t.Fatalf("master: %d, want 200 naming both tracks:\n%s", code, master)
			}
			for _, rep := range cencReps {
				code, pl := get(t, h, "/v1/channels/c/media/"+rep.id+".m3u8")
				if n := strings.Count(string(pl), "#EXTINF:"); code != http.StatusOK || n != rep.segments {
					t.Errorf("%s playlist: %d with %d segments, want 200 with %d:\n%s", rep.id, code, n, rep.segments, pl)
				}
			}

			want := []dashdecrypt.Warning{{Kind: dashdecrypt.KindKeyUnmatched,
				KIDs: []string{"30303030303030303030303030303030"}}}
			got := decryptWarnings(t, mgr)
			if len(got) != 1 || got[0].Kind != want[0].Kind || !slices.Equal(got[0].KIDs, want[0].KIDs) {
				t.Errorf("decrypt warnings = %+v, want only key_unmatched for the unused key", got)
			}
		})
	}
}

// wrmHeaderUTF16 is a PlayReady header object as an mspr:pro element carries
// it: UTF-16LE, base64.
func wrmHeaderUTF16(laURL string) string {
	xml := `<WRMHEADER xmlns="http://schemas.microsoft.com/DRM/2007/03/PlayReadyHeader" version="4.0.0.0">` +
		`<DATA><PROTECTINFO><KEYLEN>16</KEYLEN><ALGID>AESCTR</ALGID></PROTECTINFO>` +
		`<KID>ICAgICAgICAgICAgICAgIA==</KID><LA_URL>` + laURL + `</LA_URL></DATA></WRMHEADER>`
	var b []byte
	for _, u := range utf16.Encode([]rune(xml)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func laURLs(t *testing.T, mpd string) []string {
	t.Helper()
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<mspr:pro>(.*?)</mspr:pro>`).FindAllStringSubmatch(mpd, -1) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m[1]))
		if err != nil || len(raw)%2 != 0 {
			t.Fatalf("pro blob does not decode: %v", err)
		}
		u := make([]uint16, len(raw)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(raw[2*i:])
		}
		s := string(utf16.Decode(u))
		if _, rest, ok := strings.Cut(s, "<LA_URL>"); ok {
			url, _, _ := strings.Cut(rest, "</LA_URL>")
			out = append(out, url)
		}
	}
	return out
}

// With only the video key, the video AdaptationSet is decrypted and the audio
// one stays exactly as the origin serves it: ciphertext, its
// ContentProtection kept and its PlayReady licence URL pointing at the
// gateway's proxy. The channel is not served as HLS, and status says why.
func TestDASHDecryptMissingKeyKeepsAdaptationSetEncrypted(t *testing.T) {
	const licenseServer = "https://license.example/rightsmanager.asmx"
	addPlayReady := func(mpd string) string {
		mpd = strings.Replace(mpd, `xmlns:cenc="urn:mpeg:cenc:2013"`,
			`xmlns:cenc="urn:mpeg:cenc:2013" xmlns:mspr="urn:microsoft:playready"`, 1)
		audio := strings.Index(mpd, "<!-- Audio -->")
		pr := `<ContentProtection schemeIdUri="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95" value="MSPR 2.0">` +
			`<mspr:pro>` + wrmHeaderUTF16(licenseServer) + `</mspr:pro></ContentProtection>`
		at := audio + strings.Index(mpd[audio:], "<SegmentTemplate")
		return mpd[:at] + pr + "\n      " + mpd[at:]
	}
	o := startCENCOrigin(t, "cenc", addPlayReady)
	mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey)
	waitVOD(t, mgr)

	video, audio := cencReps[0], cencReps[1]
	assertDecrypted(t, h, o, video, video.segments)

	if code, init := get(t, h, initURL(audio)); code != http.StatusOK || !bytes.Equal(init, o.fixture(t, "cenc", audio, "init.mp4")) {
		t.Errorf("audio init: %d, want 200 with the origin's bytes", code)
	}
	for i := 1; i <= audio.segments; i++ {
		file := fmt.Sprintf("seg-%d.m4s", i)
		if code, seg := get(t, h, segmentURL(audio, i)); code != http.StatusOK || !bytes.Equal(seg, o.fixture(t, "cenc", audio, file)) {
			t.Errorf("audio segment %d: %d, want 200 with the origin's ciphertext", i, code)
		}
	}

	code, body := get(t, h, "/v1/channels/c/manifest.mpd")
	mpd := string(body)
	if code != http.StatusOK {
		t.Fatalf("manifest: %d", code)
	}
	videoAS, audioAS, ok := strings.Cut(mpd, `mimeType="audio/mp4"`)
	if !ok {
		t.Fatalf("manifest has no audio AdaptationSet:\n%s", mpd)
	}
	if strings.Contains(videoAS, "ContentProtection") {
		t.Errorf("decrypted video AdaptationSet still signals protection:\n%s", mpd)
	}
	if n := strings.Count(audioAS, "<ContentProtection"); n != 3 {
		t.Errorf("encrypted audio AdaptationSet has %d ContentProtection elements, want the origin's 3:\n%s", n, mpd)
	}
	if !strings.Contains(audioAS, `cenc:default_KID="20202020-2020-2020-2020-202020202020"`) {
		t.Errorf("audio ContentProtection lost its default_KID:\n%s", mpd)
	}
	if urls := laURLs(t, mpd); !slices.Equal(urls, []string{"http://gw.test/v1/channels/c/license/playready"}) {
		t.Errorf("PlayReady LA_URLs = %q, want the gateway's licence proxy", urls)
	}

	if code, _ := get(t, h, "/v1/channels/c/master.m3u8"); code == http.StatusOK {
		t.Error("master playlist served for a channel with an encrypted AdaptationSet")
	}

	var missing, closed bool
	for _, w := range decryptWarnings(t, mgr) {
		switch w.Kind {
		case dashdecrypt.KindMissingKey:
			missing = w.AdaptationSet == "1" && slices.Equal(w.KIDs, []string{"20202020202020202020202020202020"})
		case dashdecrypt.KindHLSClosed:
			closed = w.AdaptationSet == "1"
		default:
			t.Errorf("unexpected warning %+v", w)
		}
		if strings.Contains(w.Message, "00112233445566778899aabbccddeeff") {
			t.Errorf("warning carries a key value: %+v", w)
		}
	}
	if !missing || !closed {
		t.Errorf("decrypt warnings = %+v, want missing_key for the audio KID and hls_closed for the audio AdaptationSet",
			decryptWarnings(t, mgr))
	}
}

// A restarted channel reads back which AdaptationSets it decrypted, so the
// segments it ingests after the restart are decrypted too.
func TestDASHDecryptSurvivesRestart(t *testing.T) {
	// A live origin that first lists the first two segments of each track,
	// then all of them.
	live := func(firstTwo bool) func(string) string {
		return func(mpd string) string {
			mpd = strings.Replace(mpd, `type="static"`, `type="dynamic" availabilityStartTime="1970-01-01T00:00:00Z" `+
				`minimumUpdatePeriod="PT1S" timeShiftBufferDepth="PT1H"`, 1)
			mpd = strings.Replace(mpd, ` mediaPresentationDuration="PT2.000S"`, "", 1)
			if firstTwo {
				mpd = strings.Replace(mpd, `<S d="6144" r="3"/>`, `<S d="6144" r="1"/>`, 1)
				mpd = strings.Replace(mpd, `<S d="23552"/>
          <S d="24576"/>
          <S d="768"/>`, "", 1)
			}
			return mpd
		}
	}
	o := startCENCOrigin(t, "cenc", live(true))
	dataDir := t.TempDir()
	mgr, h := newCENCChannel(t, o, dataDir, cencVideoKey, cencAudioKey)

	waitServed := func(rep cencRep, n int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if code, _ := get(t, h, segmentURL(rep, n)); code == http.StatusOK {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s segment %d never served", rep.id, n)
	}
	for _, rep := range cencReps {
		waitServed(rep, 2)
	}
	if err := mgr.Stop("c"); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(store.RepLocation{DataDir: dataDir, ChannelID: "c", MediaType: "video", ASID: "0", RepID: "v"}.ASDir(),
		dashdecrypt.StateFileName)
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("decrypted AdaptationSet has no %s: %v", dashdecrypt.StateFileName, err)
	}

	o.setEdit(live(false))
	if err := mgr.Restart(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	waitServed(cencReps[0], 4)
	waitServed(cencReps[1], 4)
	for _, rep := range cencReps {
		assertDecrypted(t, h, o, rep, 4)
	}
	if code, mpd := get(t, h, "/v1/channels/c/manifest.mpd"); code != http.StatusOK || bytes.Contains(mpd, []byte("ContentProtection")) {
		t.Errorf("live manifest after restart: %d, want 200 without ContentProtection:\n%s", code, mpd)
	}
}
