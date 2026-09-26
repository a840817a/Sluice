package manifest

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/store"
)

const localSrcMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT8S"
     profiles="urn:mpeg:dash:profile:isoff-main:2011">
  <Period id="1" duration="PT8S">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="90000" duration="180180" startNumber="1"
                       initialization="init.mp4" media="$Number%09d$.m4s"/>
      <Representation id="1" width="1920" height="1080" bandwidth="6000000" codecs="avc1.4d4028"/>
    </AdaptationSet>
  </Period>
</MPD>`

// buildLocalFixture lays a channel out on disk exactly as the store does and
// returns the channel directory plus an index that knows those files. The
// segment numbers deliberately have a hole: a template-shaped manifest cannot
// express one, and misaddressing everything after it is the failure this whole
// artifact exists to avoid.
func buildLocalFixture(t *testing.T) (channelDir string, ci *index.ChannelIndex, segNos []uint64) {
	t.Helper()
	dataDir := t.TempDir()
	const channelID = "ch"
	channelDir = filepath.Join(dataDir, channelID)

	segNos = []uint64{1, 2, 3, 1535}
	repDir := filepath.Join(channelDir, "periods", "1", "video_0", "1")
	if err := os.MkdirAll(repDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initPath := filepath.Join(repDir, store.InitFileName)
	if err := os.WriteFile(initPath, []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}

	ci = index.NewChannelIndex(index.PublishAVBarrier)
	rep := ci.Period("1").AS("0").Rep("1", 0)
	for i, n := range segNos {
		p := store.RepLocation{DataDir: dataDir, ChannelID: channelID, PeriodID: "1", MediaType: "video", ASID: "0", RepID: "1"}.SegmentPath(n, store.ExtFMP4)
		if err := os.WriteFile(p, []byte("seg"), 0o644); err != nil {
			t.Fatal(err)
		}
		rep.RestorePublished(index.SegmentState{
			SegNo:     n,
			StartPTS:  int64(i) * 180180,
			EndPTS:    int64(i+1) * 180180,
			Timescale: 90000,
			Path:      p,
			InitPath:  initPath,
		})
	}
	return channelDir, ci, segNos
}

// The point of the local manifest is that a reader opening it from disk can
// resolve every path it names. Producing well-formed XML that points at files
// which are not there is the failure mode, not a lesser success.
func TestLocalMPDReferencesOnlyFilesThatExist(t *testing.T) {
	channelDir, ci, segNos := buildLocalFixture(t)

	p, err := imdp.Parse([]byte(localSrcMPD))
	if err != nil {
		t.Fatalf("parse source MPD: %v", err)
	}

	data, err := GenerateLocalMPD(p, ci.Snapshot(), channelDir)
	if err != nil {
		t.Fatalf("GenerateLocalMPD: %v", err)
	}

	refs := localManifestRefs(t, data)
	if len(refs) != len(segNos)+1 { // segments + init
		t.Fatalf("manifest names %d paths, want %d (%d segments + init): %v",
			len(refs), len(segNos)+1, len(segNos), refs)
	}
	for _, ref := range refs {
		if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") {
			t.Errorf("%q is absolute; the whole point is that it resolves against the manifest", ref)
			continue
		}
		if _, err := os.Stat(filepath.Join(channelDir, filepath.FromSlash(ref))); err != nil {
			t.Errorf("manifest names %q, which does not exist on disk: %v", ref, err)
		}
	}

	// The hole must survive: 1535 is named as itself, not renumbered to 4.
	joined := strings.Join(refs, "\n")
	if !strings.Contains(joined, "/1535.m4s") {
		t.Errorf("segment 1535 is missing or was renumbered; refs:\n%s", joined)
	}
}

// localManifestRefs pulls every sourceURL/media attribute out of the generated
// manifest, without assuming which element carries them.
func localManifestRefs(t *testing.T, data []byte) []string {
	t.Helper()
	var refs []string
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "sourceURL" || a.Name.Local == "media" {
				refs = append(refs, a.Value)
			}
		}
	}
	return refs
}
