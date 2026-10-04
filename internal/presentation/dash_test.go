package presentation

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

func testGenerator() *Generator {
	var cfg config.Config
	cfg.Server.BaseURL = "http://gw.test"
	cfg.Window.Depth = 120 * time.Second
	cfg.Window.SafeEdgeBuffer = 6 * time.Second
	cfg.Upstream.PollInterval = 2 * time.Second
	return NewGenerator(cfg)
}

// hlsIndex holds n published fMP4 segments of one video representation of an
// HLS source, in the AdaptationSet the HLS ingest writes video to.
func hlsIndex(repID string, n int) *index.ChannelIndex {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	as := ci.Period("0").AS(hlsingest.VideoASID)
	as.SetMediaType(index.MediaVideo)
	rep := as.Rep(repID, 0)
	for i := 0; i < n; i++ {
		rep.RestorePublished(index.SegmentState{
			SegNo: uint64(i), StartPTS: int64(i) * 4000, EndPTS: int64(i+1) * 4000, Timescale: 1000,
			Path: "/d/" + repID + ".m4s", InitPath: "/d/" + repID + "/init.mp4",
		})
	}
	return ci
}

func fmp4HLSChannel(ci *index.ChannelIndex) Channel {
	return Channel{
		ID: "c1", Index: ci, IsHLSSource: true, ConfigIsHLS: true,
		StaticIngestStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Tracks: Tracks{
			Variants:  []hlsdesc.Variant{{RepID: "v0", Bandwidth: 1000000, Codecs: "avc1.4d401f", Width: 1280, Height: 720}},
			Ready:     true,
			ServesHLS: true, ServesDASH: true,
		},
	}
}

// playReadyLAURLs returns the LA_URL of every PlayReady header in doc. Each
// sits base64-encoded, as UTF-16LE, inside an <mspr:pro> element.
func playReadyLAURLs(doc string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<mspr:pro>(.*?)</mspr:pro>`).FindAllStringSubmatch(doc, -1) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m[1]))
		if err != nil || len(raw)%2 != 0 {
			continue
		}
		u := make([]uint16, len(raw)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(raw[2*i:])
		}
		hdr := string(utf16.Decode(u))
		if i := strings.Index(hdr, "<LA_URL>"); i >= 0 {
			if j := strings.Index(hdr[i:], "</LA_URL>"); j >= 0 {
				out = append(out, hdr[i+len("<LA_URL>"):i+j])
			}
		}
	}
	return out
}

func attr(t *testing.T, mpd []byte, name string) string {
	t.Helper()
	m := regexp.MustCompile(`\b` + name + `="([^"]*)"`).FindSubmatch(mpd)
	if m == nil {
		return ""
	}
	return string(m[1])
}

func TestDASHRefusesWhatTheChannelCannotServe(t *testing.T) {
	g := testGenerator()
	for _, tc := range []struct {
		name string
		ch   Channel
		want error
	}{
		{"MPEG-TS HLS source", Channel{ID: "c1", Index: hlsIndex("v0", 3), IsHLSSource: true, Tracks: Tracks{ServesHLS: true}}, ErrNotServed},
		{"DASH source before its first MPD", Channel{ID: "c1", Index: index.NewChannelIndex(index.PublishAVBarrier), Tracks: Tracks{ServesHLS: true, ServesDASH: true}}, ErrNotReady},
		{"fMP4 HLS source with nothing published", fmp4HLSChannel(index.NewChannelIndex(index.PublishAVBarrier)), ErrNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.DASHNow(tc.ch)
			if !errors.Is(err, tc.want) || out != nil {
				t.Fatalf("DASHNow = %d bytes, %v; want nil, %v", len(out), err, tc.want)
			}
			if strings.HasPrefix(err.Error(), "output not") {
				t.Errorf("error %q reads as the sentinel, want the reason a player sees", err)
			}
		})
	}
}

// An fMP4 HLS source has no MPD; the one DASH serves is synthesized from its
// variants and published segments, live until the channel is VOD.
func TestDASHSynthesizesAnHLSSourcesMPD(t *testing.T) {
	g := testGenerator()
	ch := fmp4HLSChannel(hlsIndex("v0", 5))

	live, err := g.DASHNow(ch)
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	if got := attr(t, live, "type"); got != "dynamic" {
		t.Errorf("live type = %q, want dynamic", got)
	}
	if !strings.Contains(string(live), `id="v0"`) {
		t.Errorf("live MPD does not list representation v0:\n%s", live)
	}

	ch.VOD = true
	vod, err := g.DASHNow(ch)
	if err != nil {
		t.Fatalf("vod: %v", err)
	}
	if got := attr(t, vod, "type"); got != "static" {
		t.Errorf("VOD type = %q, want static", got)
	}
	// A VOD asset lasts as long as its published segments: five of 4s.
	if got := attr(t, vod, "mediaPresentationDuration"); got != "PT20S" {
		t.Errorf("VOD mediaPresentationDuration = %q, want PT20S", got)
	}
}

// A channel that keeps every segment offers a day of time shift instead of the
// configured window.
func TestDASHWindowFollowsKeepAll(t *testing.T) {
	g := testGenerator()
	ch := fmp4HLSChannel(hlsIndex("v0", 5))

	windowed, err := g.DASHNow(ch)
	if err != nil {
		t.Fatal(err)
	}
	ch.KeepAll = true
	all, err := g.DASHNow(ch)
	if err != nil {
		t.Fatal(err)
	}
	if got := attr(t, windowed, "timeShiftBufferDepth"); got != "PT120S" {
		t.Errorf("windowed timeShiftBufferDepth = %q, want PT120S", got)
	}
	if got := attr(t, all, "timeShiftBufferDepth"); got != "PT86400S" {
		t.Errorf("keep-all timeShiftBufferDepth = %q, want PT86400S", got)
	}
}

// Only the DRM systems a channel enables are signalled, and PlayReady's
// license acquisition points at the gateway's license proxy.
const widevineSystemID = "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"

func TestDASHSignalsOnlyEnabledDRM(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	ch := Channel{
		ID: "c1", Index: index.NewChannelIndex(index.PublishAVBarrier), ParsedMPD: p,
		Tracks: TracksFromMPD(p), PlayReady: true, VOD: true,
	}

	out, err := testGenerator().DASHNow(ch)
	if err != nil {
		t.Fatal(err)
	}
	const want = "http://gw.test/v1/channels/c1/license/playready"
	urls := playReadyLAURLs(string(out))
	if len(urls) == 0 {
		t.Error("enabled PlayReady is not signalled")
	}
	for _, u := range urls {
		if u != want {
			t.Errorf("PlayReady LA_URL = %q, want the gateway's proxy %q", u, want)
		}
	}
	if strings.Contains(string(out), widevineSystemID) {
		t.Error("disabled Widevine is still signalled")
	}
}
