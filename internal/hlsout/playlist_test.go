package hlsout

import (
	"strconv"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
)

const ts90k = 90000

// seg builds a published TS segment of the given duration in seconds.
func seg(segNo uint64, startSec, durSec float64) index.SegmentState {
	return index.SegmentState{
		SegNo:     segNo,
		StartPTS:  int64(startSec * ts90k),
		EndPTS:    int64((startSec + durSec) * ts90k),
		Timescale: ts90k,
		Path:      "/data/ch1/periods/p0/video_0/v0/" + strconv.FormatUint(segNo, 10) + ".ts",
		Status:    index.StatusPublished,
	}
}

func mediaOpts() MediaOptions {
	return MediaOptions{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		RepID:          "v0",
		TargetDuration: 6,
	}
}

// lineAfter returns the line following the first line matching prefix.
func lineAfter(t *testing.T, out, prefix string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatalf("no line starting with %q in:\n%s", prefix, out)
	return ""
}

func TestMediaLiveBasics(t *testing.T) {
	segs := []index.SegmentState{
		seg(100, 0, 6),
		seg(101, 6, 6),
		seg(102, 12, 5.5),
	}
	out := string(Media(segs, mediaOpts()))

	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:6",
		"#EXT-X-MEDIA-SEQUENCE:100",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Live playlists must not be terminated.
	if strings.Contains(out, "#EXT-X-ENDLIST") {
		t.Error("live playlist must not contain EXT-X-ENDLIST")
	}
	if strings.Contains(out, "#EXT-X-PLAYLIST-TYPE") {
		t.Error("live playlist must not declare a playlist type")
	}

	// Segment URIs point back at the gateway, with the stored extension.
	wantURI := "https://gw.example.com/v1/channels/ch1/segments/v0/100.ts"
	if got := lineAfter(t, out, "#EXTINF:6.000000"); got != wantURI {
		t.Errorf("first segment URI = %q, want %q", got, wantURI)
	}
	if !strings.Contains(out, "#EXTINF:5.500000,") {
		t.Errorf("fractional EXTINF not emitted:\n%s", out)
	}
	if n := strings.Count(out, "#EXTINF:"); n != 3 {
		t.Errorf("emitted %d segments, want 3", n)
	}
}

func TestMediaLiveFollowsLiveEdgeAcrossAHole(t *testing.T) {
	// Segment 101 was permanently lost. A live playlist must keep following the
	// live edge rather than stalling at the hole forever, so only the
	// contiguous run ending at the newest segment is emitted.
	segs := []index.SegmentState{
		seg(100, 0, 6),
		seg(102, 12, 6),
		seg(103, 18, 6),
	}
	out := string(Media(segs, mediaOpts()))

	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:102") {
		t.Errorf("want MEDIA-SEQUENCE 102 (run after the hole):\n%s", out)
	}
	if n := strings.Count(out, "#EXTINF:"); n != 2 {
		t.Errorf("emitted %d segments, want 2", n)
	}
	if strings.Contains(out, "/segments/v0/100.ts") {
		t.Error("segment before the hole must not be emitted")
	}
}

func TestMediaNeverEmitsAcrossAGap(t *testing.T) {
	// Whatever the mode, the emitted run must be strictly contiguous —
	// a playlist that skips a sequence number desynchronises players.
	segs := []index.SegmentState{
		seg(1, 0, 6),
		seg(2, 6, 6),
		seg(5, 24, 6),
	}
	opts := mediaOpts()
	opts.VOD = true
	out := string(Media(segs, opts))

	first := lineAfter(t, out, "#EXTINF:6.000000")
	if !strings.HasSuffix(first, "/1.ts") {
		t.Errorf("VOD must start at the first segment, got %q", first)
	}
	if n := strings.Count(out, "#EXTINF:"); n != 2 {
		t.Errorf("emitted %d segments, want 2 (truncated at the gap)", n)
	}
	if strings.Contains(out, "/segments/v0/5.ts") {
		t.Error("segment after the gap must not be emitted")
	}
}

func TestMediaVOD(t *testing.T) {
	segs := []index.SegmentState{seg(0, 0, 4), seg(1, 4, 4)}
	opts := mediaOpts()
	opts.VOD = true
	out := string(Media(segs, opts))

	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-ENDLIST",
		"#EXT-X-MEDIA-SEQUENCE:0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestMediaWindowKeepsNewestSegments(t *testing.T) {
	var segs []index.SegmentState
	for i := uint64(0); i < 10; i++ {
		segs = append(segs, seg(100+i, float64(i)*6, 6))
	}
	opts := mediaOpts()
	opts.WindowSegments = 3
	out := string(Media(segs, opts))

	if n := strings.Count(out, "#EXTINF:"); n != 3 {
		t.Errorf("emitted %d segments, want 3", n)
	}
	// The window slides with the live edge: newest three are 107,108,109.
	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:107") {
		t.Errorf("want MEDIA-SEQUENCE 107:\n%s", out)
	}
	if !strings.Contains(out, "/segments/v0/109.ts") {
		t.Error("newest segment must be present")
	}
	if strings.Contains(out, "/segments/v0/106.ts") {
		t.Error("segment outside the window must be dropped")
	}
}

func TestMediaFMP4EmitsMapAndVersion6(t *testing.T) {
	s := seg(1, 0, 4)
	s.Path = "/data/ch1/periods/p0/video_0/v0/1.m4s"
	opts := mediaOpts()
	opts.InitURI = "https://gw.example.com/v1/channels/ch1/init/v0.mp4"
	out := string(Media([]index.SegmentState{s}, opts))

	if !strings.Contains(out, "#EXT-X-VERSION:6") {
		t.Errorf("fMP4 playlist must declare version 6:\n%s", out)
	}
	want := `#EXT-X-MAP:URI="https://gw.example.com/v1/channels/ch1/init/v0.mp4"`
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "/segments/v0/1.m4s") {
		t.Errorf("fMP4 segment must keep its .m4s extension:\n%s", out)
	}
}

func TestMediaEmitsDiscontinuity(t *testing.T) {
	s1, s2 := seg(10, 0, 6), seg(11, 6, 6)
	s2.Discontinuity = true
	opts := mediaOpts()
	opts.DiscontinuitySequence = 4
	out := string(Media([]index.SegmentState{s1, s2}, opts))

	if !strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE:4") {
		t.Errorf("missing discontinuity sequence:\n%s", out)
	}
	idxDisc := strings.Index(out, "#EXT-X-DISCONTINUITY\n")
	idxSeg11 := strings.Index(out, "/segments/v0/11.ts")
	if idxDisc < 0 || idxDisc > idxSeg11 {
		t.Errorf("EXT-X-DISCONTINUITY must precede segment 11:\n%s", out)
	}
	if strings.Count(out, "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Errorf("expected exactly one discontinuity marker:\n%s", out)
	}
}

func TestMediaEmptyInput(t *testing.T) {
	out := string(Media(nil, mediaOpts()))
	if !strings.Contains(out, "#EXTM3U") {
		t.Error("empty playlist must still be well-formed")
	}
	if strings.Contains(out, "#EXTINF:") {
		t.Error("empty playlist must contain no segments")
	}
}

func TestStickyTargetDurationNeverDecreases(t *testing.T) {
	// Advertising a smaller TARGETDURATION than before violates RFC 8216 and
	// produces intermittent player stalls, so the value only ever grows.
	var st StickyTargetDuration

	if got := st.Get("v0", []index.SegmentState{seg(1, 0, 6.0)}); got != 6 {
		t.Errorf("initial target duration = %d, want 6", got)
	}
	// A longer segment raises it (ceil of the max EXTINF).
	if got := st.Get("v0", []index.SegmentState{seg(2, 6, 9.5)}); got != 10 {
		t.Errorf("target duration = %d, want 10", got)
	}
	// Shorter segments must not lower it.
	if got := st.Get("v0", []index.SegmentState{seg(3, 16, 2.0)}); got != 10 {
		t.Errorf("target duration dropped to %d, want it held at 10", got)
	}
	// Tracking is per representation.
	if got := st.Get("v1", []index.SegmentState{seg(1, 0, 4.0)}); got != 4 {
		t.Errorf("independent rep target duration = %d, want 4", got)
	}
}

func TestMaster(t *testing.T) {
	variants := []hlsdesc.Variant{
		{RepID: "v720", Bandwidth: 1280000, AverageBandwidth: 1000000,
			Codecs: "avc1.4d401f,mp4a.40.2", Width: 1280, Height: 720, FrameRate: 30},
		{RepID: "v360", Bandwidth: 640000, Codecs: "avc1.4d401e", Width: 640, Height: 360},
	}
	out := string(Master(variants, MasterOptions{
		GatewayBaseURL:      "https://gw.example.com",
		ChannelID:           "ch1",
		IndependentSegments: true,
	}))

	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-INDEPENDENT-SEGMENTS",
		`BANDWIDTH=1280000`,
		`AVERAGE-BANDWIDTH=1000000`,
		`RESOLUTION=1280x720`,
		`CODECS="avc1.4d401f,mp4a.40.2"`,
		"FRAME-RATE=30.000",
		"https://gw.example.com/v1/channels/ch1/media/v720.m3u8",
		"https://gw.example.com/v1/channels/ch1/media/v360.m3u8",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A variant without AVERAGE-BANDWIDTH or FRAME-RATE must omit them rather
	// than emit zeros.
	if strings.Contains(out, "AVERAGE-BANDWIDTH=0") || strings.Contains(out, "FRAME-RATE=0") {
		t.Errorf("zero-valued optional attributes must be omitted:\n%s", out)
	}
}

// encSeg builds a published, still-encrypted TS segment.
func encSeg(segNo uint64, startSec, durSec float64, keyURI string, iv []byte) index.SegmentState {
	s := seg(segNo, startSec, durSec)
	s.KeyURI = keyURI
	s.IV = iv
	return s
}

func testIV(b byte) []byte {
	iv := make([]byte, 16)
	iv[15] = b
	return iv
}

func keyOpts() MediaOptions {
	o := mediaOpts()
	o.KeyProxyBase = "https://gw.example.com/v1/channels/ch1/key"
	return o
}

func TestMediaEmitsKeyForEncryptedSegments(t *testing.T) {
	segs := []index.SegmentState{
		encSeg(0, 0, 6, "https://drm.example.com/k1.bin", testIV(1)),
		encSeg(1, 6, 6, "https://drm.example.com/k1.bin", testIV(2)),
	}
	out := string(Media(segs, keyOpts()))

	if !strings.Contains(out, "#EXT-X-KEY:METHOD=AES-128") {
		t.Fatalf("no EXT-X-KEY emitted:\n%s", out)
	}
	// The key URI must point at the gateway proxy, never at the upstream server.
	if strings.Contains(out, "drm.example.com") {
		t.Errorf("playlist leaks the upstream key URL:\n%s", out)
	}
	if !strings.Contains(out, "https://gw.example.com/v1/channels/ch1/key/") {
		t.Errorf("key URI does not point at the gateway proxy:\n%s", out)
	}
	// An explicit IV means the output never depends on our segment numbering
	// matching the upstream's.
	if !strings.Contains(out, "IV=0x00000000000000000000000000000001") {
		t.Errorf("first segment IV not emitted explicitly:\n%s", out)
	}
	// The key line must precede the first segment URI.
	if strings.Index(out, "#EXT-X-KEY") > strings.Index(out, "/segments/v0/0.ts") {
		t.Errorf("EXT-X-KEY must come before the segment it applies to:\n%s", out)
	}
}

func TestMediaReEmitsKeyOnRotationAndPerDistinctIV(t *testing.T) {
	segs := []index.SegmentState{
		encSeg(0, 0, 6, "https://drm.example.com/k1.bin", testIV(1)),
		encSeg(1, 6, 6, "https://drm.example.com/k1.bin", testIV(2)),
		encSeg(2, 12, 6, "https://drm.example.com/k2.bin", testIV(3)),
	}
	out := string(Media(segs, keyOpts()))

	// Every segment has its own IV, so each needs its own EXT-X-KEY line.
	if n := strings.Count(out, "#EXT-X-KEY:METHOD=AES-128"); n != 3 {
		t.Errorf("emitted %d key lines, want 3 (one per distinct IV):\n%s", n, out)
	}
	// The rotation to a second key must produce a different proxy URI.
	id1 := hlskey.KeyID("https://drm.example.com/k1.bin")
	id2 := hlskey.KeyID("https://drm.example.com/k2.bin")
	if !strings.Contains(out, id1) || !strings.Contains(out, id2) {
		t.Errorf("both key IDs must appear after rotation:\n%s", out)
	}
}

func TestMediaEmitsKeyNoneWhenEncryptionStops(t *testing.T) {
	segs := []index.SegmentState{
		encSeg(0, 0, 6, "https://drm.example.com/k1.bin", testIV(1)),
		seg(1, 6, 6), // clear
	}
	out := string(Media(segs, keyOpts()))

	if !strings.Contains(out, "#EXT-X-KEY:METHOD=NONE") {
		t.Errorf("METHOD=NONE not emitted when encryption stops:\n%s", out)
	}
	if strings.Index(out, "METHOD=NONE") > strings.Index(out, "/segments/v0/1.ts") {
		t.Errorf("METHOD=NONE must precede the first clear segment:\n%s", out)
	}
}

func TestMediaEmitsNoKeyForCleartextSegments(t *testing.T) {
	// Decrypt-on-ingest channels store cleartext, so the playlist must carry no
	// key information at all.
	out := string(Media([]index.SegmentState{seg(0, 0, 6), seg(1, 6, 6)}, keyOpts()))
	if strings.Contains(out, "EXT-X-KEY") {
		t.Errorf("cleartext playlist must not contain EXT-X-KEY:\n%s", out)
	}
}

func TestMediaOmitsKeyWithoutProxyBase(t *testing.T) {
	// Without a proxy base there is no safe URI to publish, so the key must be
	// left out rather than pointing players at the upstream key server.
	o := mediaOpts()
	o.KeyProxyBase = ""
	out := string(Media([]index.SegmentState{
		encSeg(0, 0, 6, "https://drm.example.com/k1.bin", testIV(1)),
	}, o))

	if strings.Contains(out, "drm.example.com") {
		t.Errorf("upstream key URL leaked when no proxy base was configured:\n%s", out)
	}
}

func TestMasterWithDemuxedAudio(t *testing.T) {
	variants := []hlsdesc.Variant{
		{RepID: "v0", Bandwidth: 1280000, Codecs: "avc1.4d401f", Width: 1280, Height: 720, AudioGroup: "aac"},
	}
	out := string(Master(variants, MasterOptions{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		Renditions: []hlsdesc.Rendition{
			{RepID: "a0", GroupID: "aac", Name: "English", Language: "en", Default: true, Channels: "2"},
			{RepID: "a1", GroupID: "aac", Name: "日本語", Language: "ja"},
		},
	}))

	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",CHANNELS="2",DEFAULT=YES,AUTOSELECT=YES,URI="https://gw.example.com/v1/channels/ch1/media/a0.m3u8"`,
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="日本語",LANGUAGE="ja",URI="https://gw.example.com/v1/channels/ch1/media/a1.m3u8"`,
		`AUDIO="aac"`,
		"https://gw.example.com/v1/channels/ch1/media/v0.m3u8",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("master missing %q:\n%s", want, out)
		}
	}
	// The audio media declarations must precede the variants that reference them.
	if strings.Index(out, "EXT-X-MEDIA") > strings.Index(out, "EXT-X-STREAM-INF") {
		t.Errorf("EXT-X-MEDIA must come before EXT-X-STREAM-INF:\n%s", out)
	}
}

func TestMasterWithoutRenditionsUnchanged(t *testing.T) {
	// A muxed channel (no renditions) must not emit EXT-X-MEDIA or an AUDIO attr.
	out := string(Master([]hlsdesc.Variant{{RepID: "v0", Bandwidth: 800000}}, MasterOptions{
		GatewayBaseURL: "https://gw.example.com", ChannelID: "ch1",
	}))
	if strings.Contains(out, "EXT-X-MEDIA") || strings.Contains(out, "AUDIO=") {
		t.Errorf("muxed master must not mention audio renditions:\n%s", out)
	}
}

func TestMasterWithSubtitles(t *testing.T) {
	variants := []hlsdesc.Variant{
		{RepID: "v0", Bandwidth: 800000, Codecs: "avc1.4d401f", AudioGroup: "aac", SubtitleGroup: "subs"},
	}
	out := string(Master(variants, MasterOptions{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		Renditions:     []hlsdesc.Rendition{{RepID: "a0", GroupID: "aac", Name: "English", Language: "en", Default: true}},
		Subtitles: []hlsdesc.Subtitle{
			{RepID: "s0", GroupID: "subs", Name: "English", Language: "en", Default: true},
			{RepID: "s1", GroupID: "subs", Name: "Forced", Language: "en", Forced: true},
		},
	}))

	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="https://gw.example.com/v1/channels/ch1/media/s0.m3u8"`,
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Forced",LANGUAGE="en",FORCED=YES,URI="https://gw.example.com/v1/channels/ch1/media/s1.m3u8"`,
		`SUBTITLES="subs"`,
		`AUDIO="aac"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("master missing %q:\n%s", want, out)
		}
	}
}

func TestMediaVTTSegmentExtension(t *testing.T) {
	// A WebVTT subtitle segment must be referenced with its .vtt extension.
	s := seg(0, 0, 4)
	s.Path = "/data/ch1/periods/0/text_2/s0/0.vtt"
	out := string(Media([]index.SegmentState{s}, MediaOptions{
		GatewayBaseURL: "https://gw.example.com", ChannelID: "ch1", RepID: "s0", TargetDuration: 4,
	}))
	if !strings.Contains(out, "/segments/s0/0.vtt") {
		t.Errorf("subtitle playlist must reference the .vtt segment:\n%s", out)
	}
	if strings.Contains(out, "EXT-X-MAP") {
		t.Errorf("WebVTT playlist must not carry an init map:\n%s", out)
	}
}
