package hlssrc

import (
	"net/url"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse base URL %q: %v", raw, err)
	}
	return u
}

func TestIsMaster(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nv0.m3u8\n"
	media := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg1.ts\n"
	// A media playlist whose segment URI merely contains the tag name as text
	// must not be misdetected (the substring check the JS reference used).
	tricky := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\n#EXT-X-STREAM-INF.ts\n"

	if !IsMaster([]byte(master)) {
		t.Error("master playlist not detected")
	}
	if IsMaster([]byte(media)) {
		t.Error("media playlist misdetected as master")
	}
	if IsMaster([]byte(tricky)) {
		t.Error("segment URI containing the tag name misdetected as master")
	}
}

func TestParseMasterVariantsAndRenditions(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English, US",LANGUAGE="en",DEFAULT=YES,URI="audio/en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1280000,AVERAGE-BANDWIDTH=1000000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac"
v720/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=640000,RESOLUTION=640x360,CODECS="avc1.4d401e"
../v360/index.m3u8
`
	m, err := ParseMaster([]byte(raw), mustURL(t, "https://cdn.example.com/live/a/master.m3u8"))
	if err != nil {
		t.Fatalf("ParseMaster: %v", err)
	}
	if m.Version != 4 {
		t.Errorf("Version = %d, want 4", m.Version)
	}
	if !m.IndependentSegments {
		t.Error("IndependentSegments not set")
	}
	if len(m.Variants) != 2 {
		t.Fatalf("len(Variants) = %d, want 2", len(m.Variants))
	}

	v := m.Variants[0]
	if v.Bandwidth != 1280000 || v.AverageBandwidth != 1000000 {
		t.Errorf("bandwidths = %d/%d", v.Bandwidth, v.AverageBandwidth)
	}
	if v.Width != 1280 || v.Height != 720 {
		t.Errorf("resolution = %dx%d, want 1280x720", v.Width, v.Height)
	}
	// The comma inside the quoted CODECS value must not split the attribute list.
	if v.Codecs != "avc1.4d401f,mp4a.40.2" {
		t.Errorf("Codecs = %q, want the full quoted value", v.Codecs)
	}
	if v.AudioGroup != "aac" {
		t.Errorf("AudioGroup = %q, want aac", v.AudioGroup)
	}
	if v.URI != "https://cdn.example.com/live/a/v720/index.m3u8" {
		t.Errorf("variant 0 URI = %q", v.URI)
	}
	// "../" must actually walk up a directory — the JS reference got this wrong.
	if got := m.Variants[1].URI; got != "https://cdn.example.com/live/v360/index.m3u8" {
		t.Errorf("variant 1 URI = %q, want ../ resolved", got)
	}

	if len(m.Renditions) != 1 {
		t.Fatalf("len(Renditions) = %d, want 1", len(m.Renditions))
	}
	r := m.Renditions[0]
	if r.Type != "AUDIO" || r.GroupID != "aac" || r.Language != "en" || !r.Default {
		t.Errorf("rendition = %+v", r)
	}
	// A quoted NAME containing a comma must survive attribute splitting.
	if r.Name != "English, US" {
		t.Errorf("rendition Name = %q, want %q", r.Name, "English, US")
	}
	if r.URI != "https://cdn.example.com/live/a/audio/en.m3u8" {
		t.Errorf("rendition URI = %q", r.URI)
	}
}

func TestParseMediaBasicAndSequencing(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:6.006,
seg100.ts
#EXTINF:5.994,first title
/abs/seg101.ts
#EXT-X-DISCONTINUITY
#EXTINF:6.000,
https://other.example.com/seg102.ts
`
	m, err := ParseMedia([]byte(raw), mustURL(t, "https://cdn.example.com/live/a/index.m3u8"))
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if m.TargetDuration != 6 {
		t.Errorf("TargetDuration = %d, want 6", m.TargetDuration)
	}
	if m.MediaSequence != 100 {
		t.Errorf("MediaSequence = %d, want 100", m.MediaSequence)
	}
	if m.EndList {
		t.Error("EndList set on a live playlist")
	}
	if len(m.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(m.Segments))
	}

	// SeqNo must be MediaSequence + index; everything downstream keys off this.
	for i, want := range []uint64{100, 101, 102} {
		if got := m.Segments[i].SeqNo; got != want {
			t.Errorf("Segments[%d].SeqNo = %d, want %d", i, got, want)
		}
	}
	if m.Segments[0].Duration != 6.006 {
		t.Errorf("Segments[0].Duration = %v, want 6.006", m.Segments[0].Duration)
	}
	if m.Segments[1].Title != "first title" {
		t.Errorf("Segments[1].Title = %q", m.Segments[1].Title)
	}

	// Relative, root-relative, and absolute URIs must all resolve correctly.
	wantURIs := []string{
		"https://cdn.example.com/live/a/seg100.ts",
		"https://cdn.example.com/abs/seg101.ts",
		"https://other.example.com/seg102.ts",
	}
	for i, want := range wantURIs {
		if got := m.Segments[i].URI; got != want {
			t.Errorf("Segments[%d].URI = %q, want %q", i, got, want)
		}
	}

	if m.Segments[1].Discontinuity {
		t.Error("Segments[1] should not carry a discontinuity")
	}
	if !m.Segments[2].Discontinuity {
		t.Error("Segments[2] should carry the discontinuity")
	}
}

func TestParseMediaEndListAndPlaylistType(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:4.0,
a.ts
#EXT-X-ENDLIST
`
	m, err := ParseMedia([]byte(raw), mustURL(t, "https://e.com/x/i.m3u8"))
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if !m.EndList {
		t.Error("EndList not set")
	}
	if m.PlaylistType != "VOD" {
		t.Errorf("PlaylistType = %q, want VOD", m.PlaylistType)
	}
	// Absent EXT-X-MEDIA-SEQUENCE defaults to 0.
	if m.Segments[0].SeqNo != 0 {
		t.Errorf("SeqNo = %d, want 0", m.Segments[0].SeqNo)
	}
}

func TestParseMediaKeyAppliesToFollowingSegments(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:7
#EXTINF:6.0,
clear.ts
#EXT-X-KEY:METHOD=AES-128,URI="../keys/k1.bin",IV=0x0123456789ABCDEF0123456789ABCDEF
#EXTINF:6.0,
enc1.ts
#EXTINF:6.0,
enc2.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:6.0,
clear2.ts
`
	m, err := ParseMedia([]byte(raw), mustURL(t, "https://cdn.example.com/live/a/index.m3u8"))
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(m.Segments) != 4 {
		t.Fatalf("len(Segments) = %d, want 4", len(m.Segments))
	}
	if m.Segments[0].Key != nil {
		t.Error("segment before EXT-X-KEY must be clear")
	}
	// The key applies to every segment until the next EXT-X-KEY.
	for _, i := range []int{1, 2} {
		k := m.Segments[i].Key
		if k == nil {
			t.Fatalf("Segments[%d].Key is nil", i)
		}
		if k.Method != "AES-128" {
			t.Errorf("Segments[%d] method = %q", i, k.Method)
		}
		if k.URI != "https://cdn.example.com/live/keys/k1.bin" {
			t.Errorf("Segments[%d] key URI = %q", i, k.URI)
		}
	}
	// METHOD=NONE clears the running key.
	if m.Segments[3].Key != nil {
		t.Error("METHOD=NONE must clear the running key")
	}

	iv := m.Segments[1].EffectiveIV()
	want := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF, 0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF}
	if string(iv) != string(want) {
		t.Errorf("EffectiveIV = %x, want %x", iv, want)
	}
}

func TestEffectiveIVDerivedFromSequenceNumber(t *testing.T) {
	// RFC 8216 §5.2: absent IV, use the media sequence number as a 128-bit
	// big-endian value. The JS reference omitted this and simply failed.
	raw := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:258
#EXT-X-KEY:METHOD=AES-128,URI="k.bin"
#EXTINF:6.0,
a.ts
`
	m, err := ParseMedia([]byte(raw), mustURL(t, "https://e.com/x/i.m3u8"))
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	seg := m.Segments[0]
	if seg.Key == nil || len(seg.Key.IV) != 0 {
		t.Fatalf("expected a key with no explicit IV, got %+v", seg.Key)
	}
	iv := seg.EffectiveIV()
	if len(iv) != 16 {
		t.Fatalf("len(IV) = %d, want 16", len(iv))
	}
	// 258 = 0x0102
	want := make([]byte, 16)
	want[14], want[15] = 0x01, 0x02
	if string(iv) != string(want) {
		t.Errorf("derived IV = %x, want %x", iv, want)
	}
}

func TestParseMediaMapAndByteRange(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
#EXT-X-BYTERANGE:1000@2000
body.m4s
#EXTINF:4.0,
#EXT-X-BYTERANGE:500
body.m4s
`
	m, err := ParseMedia([]byte(raw), mustURL(t, "https://e.com/x/i.m3u8"))
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if m.Segments[0].Map == nil || m.Segments[0].Map.URI != "https://e.com/x/init.mp4" {
		t.Errorf("Map = %+v", m.Segments[0].Map)
	}
	br := m.Segments[0].ByteRange
	if br == nil || br.Length != 1000 || br.Offset != 2000 {
		t.Fatalf("ByteRange[0] = %+v", br)
	}
	// An offset-less EXT-X-BYTERANGE continues from the previous sub-range.
	br = m.Segments[1].ByteRange
	if br == nil || br.Length != 500 || br.Offset != 3000 {
		t.Errorf("ByteRange[1] = %+v, want length 500 offset 3000", br)
	}
}

func TestParseMediaRejectsNonPlaylist(t *testing.T) {
	if _, err := ParseMedia([]byte("<html>nope</html>"), mustURL(t, "https://e.com/i.m3u8")); err == nil {
		t.Error("expected an error for input missing the #EXTM3U tag")
	}
}
