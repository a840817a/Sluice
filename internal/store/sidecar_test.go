package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func meta(segNo uint64, start, end int64) SegmentMeta {
	return SegmentMeta{
		SegNo:     segNo,
		StartPTS:  start,
		EndPTS:    end,
		Timescale: 90000,
	}
}

func TestSidecarAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segments.jsonl")

	want := []SegmentMeta{
		meta(10, 0, 540000),
		meta(11, 540000, 1080000),
		meta(12, 1080000, 1620000),
	}
	for _, m := range want {
		if err := AppendSegmentMeta(path, m); err != nil {
			t.Fatalf("AppendSegmentMeta: %v", err)
		}
	}

	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSidecarRoundTripsAllFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segments.jsonl")

	want := SegmentMeta{
		SegNo:         7,
		StartPTS:      12345,
		EndPTS:        67890,
		Timescale:     90000,
		Discontinuity: true,
		KeyURI:        "https://drm.example.com/k1.bin",
		IV:            "000102030405060708090a0b0c0d0e0f",
	}
	if err := AppendSegmentMeta(path, want); err != nil {
		t.Fatalf("AppendSegmentMeta: %v", err)
	}

	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestSidecarMissingFileIsNotAnError(t *testing.T) {
	// A representation that has never committed a segment simply has no
	// sidecar; that must read as empty rather than failing the whole rebuild.
	got, err := ReadSegmentMeta(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("ReadSegmentMeta on a missing file = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records, want 0", len(got))
	}
}

func TestSidecarSkipsTornFinalLine(t *testing.T) {
	// A crash mid-append leaves a partial line. Everything written before it
	// must still be recoverable.
	path := filepath.Join(t.TempDir(), "segments.jsonl")
	for _, m := range []SegmentMeta{meta(1, 0, 100), meta(2, 100, 200)} {
		if err := AppendSegmentMeta(path, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString(`{"n":3,"s":200,"e":`) // truncated mid-object
	f.Close()

	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recovered %d records, want 2 (torn line skipped)", len(got))
	}
	if got[1].SegNo != 2 {
		t.Errorf("last intact record = %d, want 2", got[1].SegNo)
	}
}

func TestSidecarSkipsGarbageLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segments.jsonl")
	content := `{"n":1,"s":0,"e":100,"t":90000}
not json at all
{"n":2,"s":100,"e":200,"t":90000}

`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recovered %d records, want 2", len(got))
	}
}

func TestSidecarLaterRecordWinsForSameSegment(t *testing.T) {
	// A segment refetched after an expiry is appended again. The newest record
	// must be the one that survives.
	path := filepath.Join(t.TempDir(), "segments.jsonl")
	if err := AppendSegmentMeta(path, meta(5, 0, 100)); err != nil {
		t.Fatal(err)
	}
	if err := AppendSegmentMeta(path, meta(5, 999, 1999)); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	byID := SegmentMetaByNo(got)
	if len(byID) != 1 {
		t.Fatalf("SegmentMetaByNo returned %d entries, want 1", len(byID))
	}
	if byID[5].StartPTS != 999 {
		t.Errorf("StartPTS = %d, want the later record's 999", byID[5].StartPTS)
	}
}

func TestCompactSegmentMetaDropsVanishedSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segments.jsonl")
	for i := uint64(1); i <= 5; i++ {
		if err := AppendSegmentMeta(path, meta(i, int64(i)*100, int64(i+1)*100)); err != nil {
			t.Fatal(err)
		}
	}

	// Segments 1 and 2 were expired off disk.
	keep := func(segNo uint64) bool { return segNo >= 3 }
	if err := CompactSegmentMeta(path, keep); err != nil {
		t.Fatalf("CompactSegmentMeta: %v", err)
	}

	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("after compaction %d records, want 3", len(got))
	}
	for _, m := range got {
		if m.SegNo < 3 {
			t.Errorf("record %d survived compaction", m.SegNo)
		}
	}

	// The file must still be appendable and readable afterwards.
	if err := AppendSegmentMeta(path, meta(6, 600, 700)); err != nil {
		t.Fatalf("append after compaction: %v", err)
	}
	got, _ = ReadSegmentMeta(path)
	if len(got) != 4 {
		t.Errorf("after post-compaction append: %d records, want 4", len(got))
	}
}

func TestCompactSegmentMetaDedupes(t *testing.T) {
	// Compaction is also what bounds the file: repeated records for one segment
	// collapse to the newest.
	path := filepath.Join(t.TempDir(), "segments.jsonl")
	for i := 0; i < 10; i++ {
		if err := AppendSegmentMeta(path, meta(1, int64(i), int64(i)+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := CompactSegmentMeta(path, func(uint64) bool { return true }); err != nil {
		t.Fatalf("CompactSegmentMeta: %v", err)
	}

	got, err := ReadSegmentMeta(path)
	if err != nil {
		t.Fatalf("ReadSegmentMeta: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after compaction %d records, want 1", len(got))
	}
	if got[0].StartPTS != 9 {
		t.Errorf("kept StartPTS %d, want the newest (9)", got[0].StartPTS)
	}
}

func TestCompactSegmentMetaOnMissingFile(t *testing.T) {
	err := CompactSegmentMeta(filepath.Join(t.TempDir(), "absent.jsonl"), func(uint64) bool { return true })
	if err != nil {
		t.Errorf("compacting a missing sidecar = %v, want nil", err)
	}
}

func TestSidecarPathSitsBesideSegments(t *testing.T) {
	segDir := filepath.Dir(SegmentPathExt("/data", "ch1", "p0", "video", "0", "v0", 1, ExtTS))
	side := SidecarPath("/data", "ch1", "p0", "video", "0", "v0")
	if filepath.Dir(side) != segDir {
		t.Errorf("sidecar dir = %q, want it beside the segments at %q", filepath.Dir(side), segDir)
	}
	if !strings.HasSuffix(side, ".jsonl") {
		t.Errorf("sidecar path = %q, want a .jsonl file", side)
	}
	// It must not be mistaken for a segment by the directory scanners.
	if _, _, ok := ParseSegmentFile(filepath.Base(side)); ok {
		t.Error("sidecar filename parses as a segment")
	}
}
