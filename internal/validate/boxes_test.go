package validate

import (
	"encoding/binary"
	"testing"
)

// box builds a top-level ISO BMFF box with a 32-bit size field.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// extBox builds a box that declares a 64-bit extended size, which is the
// `size == 1` escape hatch in ISO/IEC 14496-12.
func extBox(typ string, declaredSize uint64, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(b[0:4], 1) // size == 1: real size follows the type
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], declaredSize)
	copy(b[16:], payload)
	return b
}

func TestFindMoovRange(t *testing.T) {
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00"))
	moov := box("moov", make([]byte, 40))

	t.Run("finds moov after ftyp", func(t *testing.T) {
		data := append(append([]byte{}, ftyp...), moov...)
		start, end, ok := FindMoovRange(data)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		wantStart := uint64(len(ftyp))
		wantEnd := wantStart + uint64(len(moov)) - 1
		if start != wantStart || end != wantEnd {
			t.Errorf("range = (%d, %d), want (%d, %d)", start, end, wantStart, wantEnd)
		}
	})

	t.Run("moov beyond probe window is a retry, not a hit", func(t *testing.T) {
		data := append(append([]byte{}, ftyp...), moov...)
		truncated := data[:len(data)-10] // moov header present, body cut off
		if _, _, ok := FindMoovRange(truncated); ok {
			t.Error("ok = true for a moov extending past the probe window, want false")
		}
	})

	t.Run("no moov", func(t *testing.T) {
		if _, _, ok := FindMoovRange(ftyp); ok {
			t.Error("ok = true with no moov present, want false")
		}
	})
}

// A segment fetched from an untrusted origin can declare any box size it likes.
// Walking past such a box must not wrap the offset around uint64 — the scan runs
// on an ingest goroutine with no recover, so a panic here takes the gateway down.
func TestFindMoovRangeRejectsOverflowingBoxSize(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "64-bit extended size wraps the offset",
			data: append(
				extBox("free", 0xFFFFFFFFFFFFFFF8, make([]byte, 8)),
				box("moov", make([]byte, 16))...,
			),
		},
		{
			name: "64-bit extended size is max uint64",
			data: extBox("free", 0xFFFFFFFFFFFFFFFF, make([]byte, 8)),
		},
		{
			name: "extended size smaller than its own header",
			data: extBox("free", 2, make([]byte, 8)),
		},
		{
			name: "32-bit size overshoots the buffer",
			data: func() []byte {
				b := box("free", make([]byte, 8))
				binary.BigEndian.PutUint32(b[0:4], 0xFFFFFFFF)
				return b
			}(),
		},
		{
			name: "size 1 with no room for the extended size field",
			data: []byte{0, 0, 0, 1, 'f', 'r', 'e', 'e', 0, 0},
		},
		{
			name: "size zero means rest of file",
			data: append([]byte{0, 0, 0, 0, 'f', 'r', 'e', 'e'}, box("moov", nil)...),
		},
		{
			name: "truncated header",
			data: []byte{0, 0, 0},
		},
		{
			name: "empty",
			data: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("FindMoovRange panicked on hostile input: %v", r)
				}
			}()
			start, end, ok := FindMoovRange(tc.data)
			if ok && end < start {
				t.Errorf("returned inverted range (%d, %d)", start, end)
			}
			if ok && end >= uint64(len(tc.data)) {
				t.Errorf("returned range (%d, %d) outside the %d-byte buffer",
					start, end, len(tc.data))
			}
		})
	}
}
