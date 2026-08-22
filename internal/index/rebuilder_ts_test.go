package index

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/a840817a/sluice/internal/store"
)

const tsTimescale = 90000

// writeTSRep lays out n MPEG-TS segments plus their sidecar for one
// representation, as the processor would have on disk.
func writeTSRep(t *testing.T, dataDir, channelID string, n int, withKey bool) {
	t.Helper()
	for i := 0; i < n; i++ {
		segNo := uint64(i)
		path := store.SegmentPathExt(dataDir, channelID, "0", "video", "0", "v0", segNo, store.ExtTS)
		if err := store.Write(path, []byte{0x47, byte(i)}); err != nil {
			t.Fatalf("write segment: %v", err)
		}
		m := store.SegmentMeta{
			SegNo:         segNo,
			StartPTS:      int64(i) * 4 * tsTimescale,
			EndPTS:        int64(i+1) * 4 * tsTimescale,
			Timescale:     tsTimescale,
			Discontinuity: i == 2,
		}
		if withKey {
			m.KeyURI = "https://drm.example.com/k1.bin"
			m.IV = hex.EncodeToString([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, byte(i)})
		}
		side := store.SidecarPath(dataDir, channelID, "0", "video", "0", "v0")
		if err := store.AppendSegmentMeta(side, m); err != nil {
			t.Fatalf("append meta: %v", err)
		}
	}
}

// publishedSegs returns the published segments of the only representation.
func publishedSegs(t *testing.T, ci *ChannelIndex) []SegmentState {
	t.Helper()
	var out []SegmentState
	ci.Mu().RLock()
	defer ci.Mu().RUnlock()
	for _, ps := range ci.Periods {
		ps.Mu().RLock()
		for _, as := range ps.AdaptationSets {
			as.Mu().RLock()
			for _, rep := range as.Reps {
				out = append(out, rep.Published()...)
			}
			as.Mu().RUnlock()
		}
		ps.Mu().RUnlock()
	}
	return out
}

func TestRebuildRestoresTSSegmentsFromSidecar(t *testing.T) {
	dir := t.TempDir()
	writeTSRep(t, dir, "ch1", 5, false)

	ci := NewChannelIndex()
	if err := RebuildFromDisk(ci, dir, "ch1"); err != nil {
		t.Fatalf("RebuildFromDisk: %v", err)
	}

	segs := publishedSegs(t, ci)
	if len(segs) != 5 {
		t.Fatalf("restored %d segments, want 5", len(segs))
	}
	byNo := make(map[uint64]SegmentState, len(segs))
	for _, s := range segs {
		byNo[s.SegNo] = s
	}
	for i := uint64(0); i < 5; i++ {
		s, ok := byNo[i]
		if !ok {
			t.Fatalf("segment %d not restored", i)
		}
		if s.Timescale != tsTimescale {
			t.Errorf("segment %d Timescale = %d, want %d", i, s.Timescale, tsTimescale)
		}
		if want := int64(i) * 4 * tsTimescale; s.StartPTS != want {
			t.Errorf("segment %d StartPTS = %d, want %d", i, s.StartPTS, want)
		}
		if got := s.EndSec() - s.StartSec(); got < 3.99 || got > 4.01 {
			t.Errorf("segment %d duration = %v, want 4", i, got)
		}
		// TS segments are self-contained; an init path would be a lie.
		if s.InitPath != "" {
			t.Errorf("segment %d has InitPath %q, want empty", i, s.InitPath)
		}
	}
	// Discontinuity must survive the round trip.
	if !byNo[2].Discontinuity {
		t.Error("discontinuity flag lost across rebuild")
	}
	if byNo[1].Discontinuity {
		t.Error("discontinuity flag leaked onto a neighbouring segment")
	}
}

func TestRebuildRestoresKeyMaterial(t *testing.T) {
	// Passthrough channels must be able to re-advertise EXT-X-KEY after a
	// restart, which needs both the key URI and the per-segment IV.
	dir := t.TempDir()
	writeTSRep(t, dir, "ch1", 3, true)

	ci := NewChannelIndex()
	if err := RebuildFromDisk(ci, dir, "ch1"); err != nil {
		t.Fatalf("RebuildFromDisk: %v", err)
	}

	for _, s := range publishedSegs(t, ci) {
		if s.KeyURI != "https://drm.example.com/k1.bin" {
			t.Errorf("segment %d KeyURI = %q", s.SegNo, s.KeyURI)
		}
		if len(s.IV) != 16 {
			t.Errorf("segment %d IV is %d bytes, want 16", s.SegNo, len(s.IV))
		}
		if s.IV[15] != byte(s.SegNo) {
			t.Errorf("segment %d IV = %x, want the per-segment value", s.SegNo, s.IV)
		}
	}
}

func TestRebuildKeepsNewestTSSegmentsWhenRingIsBounded(t *testing.T) {
	// Directory listings are lexical, so "10.ts" comes before "2.ts". Committing
	// in that order into a bounded ring would retain an arbitrary subset rather
	// than the newest segments.
	dir := t.TempDir()
	writeTSRep(t, dir, "ch1", 20, false)

	ci := NewChannelIndex()
	if err := RebuildFromDiskWithCapacity(ci, dir, "ch1", 5); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	segs := publishedSegs(t, ci)
	if len(segs) != 5 {
		t.Fatalf("restored %d segments, want the ring capacity of 5", len(segs))
	}
	for _, s := range segs {
		if s.SegNo < 15 {
			t.Errorf("segment %d retained; the newest 5 (15..19) were expected", s.SegNo)
		}
	}
}

func TestRebuildSkipsTSSegmentsWithNoMetadata(t *testing.T) {
	// A .ts file whose sidecar record is missing cannot be placed on the
	// timeline; serving it would put a wrong-duration entry in the playlist.
	dir := t.TempDir()
	writeTSRep(t, dir, "ch1", 3, false)
	orphan := store.SegmentPathExt(dir, "ch1", "0", "video", "0", "v0", 99, store.ExtTS)
	if err := store.Write(orphan, []byte{0x47}); err != nil {
		t.Fatal(err)
	}

	ci := NewChannelIndex()
	if err := RebuildFromDisk(ci, dir, "ch1"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	for _, s := range publishedSegs(t, ci) {
		if s.SegNo == 99 {
			t.Error("segment with no sidecar record was restored")
		}
	}
}

func TestRebuildIgnoresSidecarFileAsSegment(t *testing.T) {
	dir := t.TempDir()
	writeTSRep(t, dir, "ch1", 2, false)

	// The sidecar sits in the same directory as the segments; it must not be
	// mistaken for one.
	side := store.SidecarPath(dir, "ch1", "0", "video", "0", "v0")
	if _, err := os.Stat(side); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	if filepath.Dir(side) != filepath.Dir(
		store.SegmentPathExt(dir, "ch1", "0", "video", "0", "v0", 0, store.ExtTS)) {
		t.Fatal("test setup: sidecar is not beside the segments")
	}

	ci := NewChannelIndex()
	if err := RebuildFromDisk(ci, dir, "ch1"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n := len(publishedSegs(t, ci)); n != 2 {
		t.Errorf("restored %d segments, want exactly 2", n)
	}
}
