package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// minimalFMP4 returns the smallest file ReadStartPTS can read: a moof holding a
// traf holding a version-1 tfdt with the given BaseMediaDecodeTime.
func minimalFMP4(baseMediaDecodeTime uint64) []byte {
	tfdt := make([]byte, 0, 20)
	tfdt = binary.BigEndian.AppendUint32(tfdt, 20)
	tfdt = append(tfdt, 't', 'f', 'd', 't')
	tfdt = append(tfdt, 1, 0, 0, 0) // version 1, no flags
	tfdt = binary.BigEndian.AppendUint64(tfdt, baseMediaDecodeTime)

	traf := make([]byte, 0, 8+len(tfdt))
	traf = binary.BigEndian.AppendUint32(traf, uint32(8+len(tfdt)))
	traf = append(traf, 't', 'r', 'a', 'f')
	traf = append(traf, tfdt...)

	moof := make([]byte, 0, 8+len(traf))
	moof = binary.BigEndian.AppendUint32(moof, uint32(8+len(traf)))
	moof = append(moof, 'm', 'o', 'o', 'f')
	moof = append(moof, traf...)
	return moof
}

// A representation whose segment numbers have a hole — a live channel that shed
// segments, or a static ingest that fetched stragglers far past its contiguous
// run — must not have the segment before the hole stretched across it. EndPTS
// comes from the next segment's StartPTS, which is only that segment's end when
// the two are actually adjacent.
//
// Before the fix the last contiguous segment absorbed the whole gap, which is
// how one audio segment came out claiming a 1423-second duration and dragged
// the VOD's mediaPresentationDuration with it.
func TestRebuildDoesNotStretchAcrossSegmentGap(t *testing.T) {
	const (
		segDur    = 96256
		straggler = 1535
	)
	dataDir := t.TempDir()
	repDir := filepath.Join(dataDir, "ch", "periods", "1", "audio_1", "5")
	if err := os.MkdirAll(repDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repDir, "init.mp4"), minimalFMP4(0), 0o644); err != nil {
		t.Fatal(err)
	}

	write := func(segNo uint64, pts uint64) {
		t.Helper()
		name := filepath.Join(repDir, itoa(segNo)+".m4s")
		if err := os.WriteFile(name, minimalFMP4(pts), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A contiguous run of three, then a straggler far ahead — the shape the
	// broker's eviction left on disk.
	write(1, 0)
	write(2, segDur)
	write(3, 2*segDur)
	write(straggler, (straggler-1)*segDur)

	ci := NewChannelIndex(PublishAVBarrier)
	if err := RebuildFromDiskForVOD(ci, dataDir, "ch"); err != nil {
		t.Fatalf("RebuildFromDiskForVOD: %v", err)
	}

	rep := ci.FindRep("1", "1", "5")
	if rep == nil {
		t.Fatal("representation not rebuilt")
	}
	var seg3 *SegmentState
	// The barrier may have moved segments to Published, so look in both.
	for _, set := range [][]SegmentState{rep.Committed(), rep.Published()} {
		for _, s := range set {
			if s.SegNo == 3 {
				c := s
				seg3 = &c
			}
		}
	}
	if seg3 == nil {
		t.Fatal("segment 3 not committed")
	}

	got := seg3.EndPTS - seg3.StartPTS
	t.Logf("segment 3: StartPTS=%d EndPTS=%d duration=%d", seg3.StartPTS, seg3.EndPTS, got)
	if got != segDur {
		t.Errorf("segment 3 duration = %d, want %d — it absorbed the gap up to segment %d",
			got, segDur, straggler)
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
