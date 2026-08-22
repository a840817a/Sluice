package dashingest

import (
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// The DASH parser and the segment index each name media types in their own
// vocabulary, and the DASH ingest path converts between them with a plain
// string(as.MediaType) — see discovery.go and watcher.go. Nothing makes the two
// agree at compile time, and a divergence would not fail loudly: the index's A/V
// barrier (barrier.go) would stop recognising a track type, fall into its
// single-track-type path, and publish video and audio without cross-track
// gating. Playback would drift out of sync with a fully green test suite.
//
// The test lives here because this is the package that performs the conversion
// (discovery.go's string(as.MediaType)), not merely one that imports both
// vocabularies — channel, httpapi and manifest do that too. If the conversion
// ever moves, this test should follow it.
func TestMediaTypeConstantsMatchMPD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dash  imdp.MediaType
		index string
	}{
		{"video", imdp.MediaVideo, index.MediaVideo},
		{"audio", imdp.MediaAudio, index.MediaAudio},
		{"text", imdp.MediaText, index.MediaText},
	} {
		if string(tc.dash) != tc.index {
			t.Errorf("%s: mpd.Media%s = %q but index.Media%s = %q — the A/V barrier keys on the index value and would silently stop gating",
				tc.name, tc.name, string(tc.dash), tc.name, tc.index)
		}
	}
}
