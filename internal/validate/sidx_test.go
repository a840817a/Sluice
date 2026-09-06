package validate

import (
	"bytes"
	"encoding/binary"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

// sidxBytes renders a sidx box — header included — the way it appears inside a
// SegmentBase representation's index range.
func sidxBytes(t *testing.T, s *mp4.Sidx) []byte {
	t.Helper()
	s.ReferenceCount = uint16(len(s.References))

	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, s, mp4.Context{}); err != nil {
		t.Fatalf("marshal sidx: %v", err)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(8+payload.Len()))
	out = append(out, 's', 'i', 'd', 'x')
	return append(out, payload.Bytes()...)
}

func ref(size, duration uint32) mp4.SidxReference {
	return mp4.SidxReference{ReferencedSize: size, SubsegmentDuration: duration, StartsWithSAP: true, SAPType: 1}
}

// A SegmentBase representation is one file; the sidx is the only thing that says
// where each segment starts and ends inside it. The offsets it yields go
// straight into HTTP Range requests, so an arithmetic slip here fetches
// overlapping or misaligned bytes and every segment fails to parse.
//
// Offsets run from the first byte after the index region, plus firstOffset,
// then accumulate each referenced size.
func TestParseSidxBytesComputesAbsoluteOffsets(t *testing.T) {
	raw := sidxBytes(t, &mp4.Sidx{
		Timescale:                  90000,
		EarliestPresentationTimeV0: 900,
		FirstOffsetV0:              0,
		References:                 []mp4.SidxReference{ref(1000, 360000), ref(2000, 360000), ref(1500, 180000)},
	})

	// indexRange was "0-835", so the media starts at 836.
	timescale, earliest, entries, err := ParseSidxBytes(raw, 836)
	if err != nil {
		t.Fatalf("ParseSidxBytes: %v", err)
	}
	if timescale != 90000 {
		t.Errorf("timescale = %d, want 90000", timescale)
	}
	if earliest != 900 {
		t.Errorf("earliest presentation time = %d, want 900", earliest)
	}

	want := []SidxEntry{
		{Offset: 836, Size: 1000, Duration: 360000},
		{Offset: 1836, Size: 2000, Duration: 360000},
		{Offset: 3836, Size: 1500, Duration: 180000},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

// firstOffset is the gap between the end of the index region and the first
// subsegment. Ignoring it shifts every byte range in the representation.
func TestParseSidxBytesHonoursFirstOffset(t *testing.T) {
	raw := sidxBytes(t, &mp4.Sidx{
		Timescale:     90000,
		FirstOffsetV0: 64,
		References:    []mp4.SidxReference{ref(500, 90000), ref(700, 90000)},
	})

	_, _, entries, err := ParseSidxBytes(raw, 1000)
	if err != nil {
		t.Fatalf("ParseSidxBytes: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Offset != 1064 {
		t.Errorf("first offset = %d, want 1064 (idxEnd 1000 + firstOffset 64)", entries[0].Offset)
	}
	if entries[1].Offset != 1564 {
		t.Errorf("second offset = %d, want 1564", entries[1].Offset)
	}
}

// A referenceType of 1 is a nested sidx, not media. It must not become a
// segment, but its size still has to advance the running offset or every
// following segment is read from the wrong place.
func TestParseSidxBytesSkipsNestedSidxButKeepsItsSize(t *testing.T) {
	nested := ref(300, 0)
	nested.ReferenceType = true

	raw := sidxBytes(t, &mp4.Sidx{
		Timescale:  90000,
		References: []mp4.SidxReference{nested, ref(1000, 90000)},
	})

	_, _, entries, err := ParseSidxBytes(raw, 100)
	if err != nil {
		t.Fatalf("ParseSidxBytes: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: the nested sidx is not a segment", len(entries))
	}
	if entries[0].Offset != 400 {
		t.Errorf("offset = %d, want 400 (idxEnd 100 plus the skipped reference's 300) — "+
			"a skipped reference must still advance the offset", entries[0].Offset)
	}
}

// Version 1 stores the presentation time and first offset as 64-bit fields.
// A long asset needs them, and reading the v0 fields instead yields zero.
func TestParseSidxBytesReadsVersion1Fields(t *testing.T) {
	s := &mp4.Sidx{
		Timescale:                  90000,
		EarliestPresentationTimeV1: 1 << 33,
		FirstOffsetV1:              1 << 32,
		References:                 []mp4.SidxReference{ref(1000, 90000)},
	}
	s.SetVersion(1)
	raw := sidxBytes(t, s)

	_, earliest, entries, err := ParseSidxBytes(raw, 0)
	if err != nil {
		t.Fatalf("ParseSidxBytes: %v", err)
	}
	if earliest != 1<<33 {
		t.Errorf("earliest = %d, want %d — the 64-bit field was not read", earliest, uint64(1)<<33)
	}
	if len(entries) != 1 || entries[0].Offset != 1<<32 {
		t.Errorf("entries = %+v, want one at offset %d", entries, uint64(1)<<32)
	}
}

// Discovery treats a parse failure as "skip this representation" rather than
// crashing the watcher, so the error path has to be an error and not a panic.
func TestParseSidxBytesRejectsWhatIsNotASidx(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"truncated header", []byte{0, 0, 0}},
		{"a different box", append(binary.BigEndian.AppendUint32(nil, 12), 'm', 'o', 'o', 'v', 0, 0, 0, 0)},
		{"garbage", []byte("this is not an mp4 box at all")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, entries, err := ParseSidxBytes(tc.data, 0)
			if err == nil {
				t.Fatalf("ParseSidxBytes accepted %s", tc.name)
			}
			if entries != nil {
				t.Errorf("entries = %+v, want none alongside an error", entries)
			}
		})
	}
}
