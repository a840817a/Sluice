package hlsout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
)

// The local playlist's whole purpose is that a reader opening it from disk can
// resolve every URI it names — and that a hole in the numbering is described
// rather than glossed over. Media deliberately truncates at the first hole;
// this must not.
func TestLocalMediaNamesEverySegmentOnDisk(t *testing.T) {
	channelDir := t.TempDir()
	repDir := filepath.Join(channelDir, "periods", "1", "video_0", "v0")
	if err := os.MkdirAll(repDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initPath := filepath.Join(repDir, "init.mp4")
	if err := os.WriteFile(initPath, []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}

	segNos := []uint64{1, 2, 3, 1535}
	var segs []index.SegmentState
	for i, n := range segNos {
		p := filepath.Join(repDir, strings.TrimSpace(itoa(n))+".m4s")
		if err := os.WriteFile(p, []byte("seg"), 0o644); err != nil {
			t.Fatal(err)
		}
		segs = append(segs, index.SegmentState{
			SegNo:     n,
			StartPTS:  int64(i) * 180180,
			EndPTS:    int64(i+1) * 180180,
			Timescale: 90000,
			Path:      p,
			InitPath:  initPath,
		})
	}

	data, err := LocalMedia(inPeriod(segs), channelDir, nil)
	if err != nil {
		t.Fatalf("LocalMedia: %v", err)
	}
	out := string(data)

	var named int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		named++
		if filepath.IsAbs(line) {
			t.Errorf("%q is absolute; it must resolve against the playlist's own directory", line)
			continue
		}
		if _, err := os.Stat(filepath.Join(channelDir, filepath.FromSlash(line))); err != nil {
			t.Errorf("playlist names %q, which does not exist on disk: %v", line, err)
		}
	}
	if named != len(segNos) {
		t.Errorf("playlist names %d segments, want %d:\n%s", named, len(segNos), out)
	}
	if !strings.Contains(out, "#EXT-X-DISCONTINUITY") {
		t.Errorf("numbering jumps 3 → 1535 with no EXT-X-DISCONTINUITY:\n%s", out)
	}
	if !strings.Contains(out, "#EXT-X-MAP:URI=") {
		t.Errorf("fMP4 segments but no EXT-X-MAP:\n%s", out)
	}
	if !strings.Contains(out, "#EXT-X-ENDLIST") {
		t.Errorf("a finished asset must be terminated:\n%s", out)
	}
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
