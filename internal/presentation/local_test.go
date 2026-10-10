package presentation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

func localPublished(t *testing.T, ci *index.ChannelIndex, dir, pid, asid, id string, no uint64, start, end int64, scale uint32, init bool, key string) {
	t.Helper()
	path := filepath.Join(dir, pid, asid, id, fmt.Sprintf("%d.seg", no))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("published media"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := index.SegmentState{SegNo: no, StartPTS: start, EndPTS: end, Timescale: scale, Path: path, KeyURI: key}
	if key != "" {
		s.IV = bytes.Repeat([]byte{byte(no)}, 16)
	}
	if init {
		s.InitPath = filepath.Join(filepath.Dir(path), "init.mp4")
		if err := os.WriteFile(s.InitPath, []byte("init"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ci.Period(pid).AS(asid).Rep(id, 0).RestorePublished(s)
}

func TestLocalHLSExportsPeriodsTracksAndOfflineKeys(t *testing.T) {
	dir := t.TempDir()
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	const keyURI = "https://origin.invalid/private-key"
	key := bytes.Repeat([]byte{0x42}, 16)
	stored, _ := json.Marshal(map[string]string{keyURI: hex.EncodeToString(key)})
	keyPath := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keyPath, stored, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := hlskey.NewCache(nil, nil, keyPath)
	for _, pid := range []string{"10", "2"} {
		for _, no := range []uint64{7, 9} {
			localPublished(t, ci, dir, pid, "0", "v0", no, 0, 2, 1, true, keyURI)
		}
		localPublished(t, ci, dir, pid, "1", "a0", 1, 0, 2, 1, false, "")
		localPublished(t, ci, dir, pid, "2", "s0", 1, 0, 2, 1, false, "")
	}
	ch := Channel{IsHLSSource: true, Tracks: Tracks{
		IndependentSegments: true,
		Variants:            []hlsdesc.Variant{{RepID: "v0", Bandwidth: 800000, AudioGroup: "audio", SubtitleGroup: "subs"}, {RepID: "missing", Bandwidth: 5}},
		Renditions:          []hlsdesc.Rendition{{RepID: "a0", GroupID: "audio", Name: "Japanese", Language: "ja", Default: true, Channels: "2"}, {RepID: "absent", GroupID: "unused", Name: "absent"}},
		Subtitles:           []hlsdesc.Subtitle{{RepID: "s0", GroupID: "subs", Name: "Traditional Chinese", Language: "zh-Hant", Forced: true}},
	}}
	files, err := testGenerator().Local(ch, ci.Snapshot(), dir, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 || files[len(files)-1].Name != "local.m3u8" {
		t.Fatalf("files = %+v", files)
	}
	contents := map[string]string{}
	keyIndex, videoIndex := -1, -1
	for i, f := range files {
		contents[f.Name] = string(f.Data)
		if strings.HasSuffix(f.Name, ".key") {
			keyIndex = i
			if f.Mode != 0o600 || !bytes.Equal(f.Data, key) {
				t.Fatal("incorrect key data or permissions")
			}
		}
		if f.Name == "local_2.m3u8" {
			videoIndex = i
		}
	}
	if keyIndex < 0 || keyIndex >= videoIndex {
		t.Fatal("key must precede referencing media")
	}
	master := contents["local.m3u8"]
	for _, want := range []string{"TYPE=AUDIO", "TYPE=SUBTITLES", `LANGUAGE="ja"`, `LANGUAGE="zh-Hant"`, `CHANNELS="2"`, `DEFAULT=YES`, `FORCED=YES`, `AUDIO="audio"`, `SUBTITLES="subs"`, "#EXT-X-INDEPENDENT-SEGMENTS"} {
		if !strings.Contains(master, want) {
			t.Errorf("missing %s in %s", want, master)
		}
	}
	if strings.Count(master, "#EXT-X-STREAM-INF:") != 1 || strings.Contains(master, "absent") {
		t.Fatal(master)
	}
	video := contents["local_2.m3u8"]
	if strings.Count(video, "#EXTINF:") != 4 || strings.Count(video, "#EXT-X-DISCONTINUITY\n") != 3 || strings.Count(video, "#EXT-X-MAP:") != 2 {
		t.Fatal(video)
	}
	if strings.Index(video, "2/0/v0/7.seg") > strings.Index(video, "10/0/v0/7.seg") {
		t.Fatal("periods not ordered numerically")
	}
	if !strings.Contains(video, "IV=0x"+strings.Repeat("09", 16)) || strings.Contains(video, "https:") {
		t.Fatal(video)
	}
	// Every media/init URI resolves on disk; each playlist/key URI is in the export.
	for name, body := range contents {
		if strings.HasSuffix(name, ".key") {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			uri := ""
			if strings.HasPrefix(line, "#") {
				if at := strings.Index(line, `URI="`); at >= 0 {
					uri = strings.Split(line[at+5:], `"`)[0]
				}
			} else {
				uri = line
			}
			if uri == "" {
				continue
			}
			if _, ok := contents[uri]; ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, uri)); err != nil {
				t.Errorf("unresolvable %s: %v", uri, err)
			}
		}
	}
	// Mutating a returned key must not corrupt the cache used for served playback.
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".key") {
			f.Data[0] = 0
		}
	}
	got, err := cache.Cached(keyURI)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("export aliases cached key")
	}
}

func TestLocalHLSOmitsUnavailableGroupsAndRejectsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	localPublished(t, ci, dir, "0", "0", "v0", 1, 0, 2, 1, false, "")
	ch := Channel{IsHLSSource: true, Tracks: Tracks{Variants: []hlsdesc.Variant{{RepID: "v0", AudioGroup: "missing", SubtitleGroup: "missing"}}}}
	files, err := testGenerator().Local(ch, ci.Snapshot(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	master := string(files[len(files)-1].Data)
	if strings.Contains(master, "AUDIO=") || strings.Contains(master, "SUBTITLES=") {
		t.Fatal(master)
	}
	localPublished(t, ci, dir, "0", "0", "v0", 2, 2, 4, 1, false, "https://unavailable.invalid/key")
	if files, err = testGenerator().Local(ch, ci.Snapshot(), dir, nil); err == nil || len(files) != 0 {
		t.Fatal("partial/unplayable encrypted export accepted")
	}
}

func TestLocalDASHRebasesPeriodsWithoutLosingTrackOffsets(t *testing.T) {
	dir := t.TempDir()
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	// Different source clocks and PTOs: audio begins half a second after video.
	// Document order is deliberately opposite lexical order.
	raw := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"><Period id="z"><AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="10" presentationTimeOffset="1000"/><Representation id="v"/></AdaptationSet><AdaptationSet mimeType="audio/mp4" lang="ja"><SegmentList timescale="2" presentationTimeOffset="400"/><Representation id="a"><SegmentList/></Representation></AdaptationSet><AdaptationSet mimeType="application/mp4" lang="zh"><SegmentBase timescale="1" presentationTimeOffset="300"/><Representation id="s" codecs="stpp"><SegmentBase/></Representation></AdaptationSet></Period><Period id="a"><AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="10"/><Representation id="v"/></AdaptationSet></Period></MPD>`
	p, err := imdp.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	localPublished(t, ci, dir, "z", "0", "v", 1, 100000, 102000, 1000, true, "")
	localPublished(t, ci, dir, "z", "0", "v", 3, 104000, 106000, 1000, true, "")
	localPublished(t, ci, dir, "z", "1", "a", 1, 401, 411, 2, true, "")
	localPublished(t, ci, dir, "z", "2", "s", 1, 301, 305, 1, true, "")
	localPublished(t, ci, dir, "a", "0", "v", 1, 500, 520, 10, true, "")
	files, err := testGenerator().Local(Channel{ParsedMPD: p}, ci.Snapshot(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "local.mpd" {
		t.Fatal(files)
	}
	var doc struct {
		Duration string `xml:"mediaPresentationDuration,attr"`
		Periods  []struct {
			ID       string `xml:"id,attr"`
			Start    string `xml:"start,attr"`
			Duration string `xml:"duration,attr"`
			AS       []struct {
				Lang string `xml:"lang,attr"`
				Rep  []struct {
					List struct {
						PTO  uint64 `xml:"presentationTimeOffset,attr"`
						URLs []struct {
							Media string `xml:"media,attr"`
						} `xml:"SegmentURL"`
					} `xml:"SegmentList"`
				} `xml:"Representation"`
			} `xml:"AdaptationSet"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal(files[0].Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Duration != "PT8.000S" || len(doc.Periods) != 2 {
		t.Fatalf("%s", files[0].Data)
	}
	first, second := doc.Periods[0], doc.Periods[1]
	if first.ID != "z" || first.Start != "PT0.000000S" || first.Duration != "PT6.000000S" || second.Start != "PT6.000000S" || second.Duration != "PT2.000000S" {
		t.Fatalf("%s", files[0].Data)
	}
	if len(first.AS) != 3 || first.AS[0].Rep[0].List.PTO != 100000 || first.AS[1].Rep[0].List.PTO != 400 || first.AS[2].Rep[0].List.PTO != 300 || second.AS[0].Rep[0].List.PTO != 500 {
		t.Fatalf("track offsets lost: %s", files[0].Data)
	}
	if len(first.AS[0].Rep[0].List.URLs) != 2 || first.AS[1].Lang != "ja" || first.AS[2].Lang != "zh" {
		t.Fatalf("%s", files[0].Data)
	}
	for _, period := range doc.Periods {
		for _, as := range period.AS {
			for _, rep := range as.Rep {
				for _, u := range rep.List.URLs {
					if _, err := os.Stat(filepath.Join(dir, u.Media)); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}
