package cenc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Errors returned by this package. Each is wrapped with detail; test with
// errors.Is.
var (
	// ErrUnsupportedScheme is a protection scheme other than cenc and cbcs.
	ErrUnsupportedScheme = errors.New("unsupported protection scheme")
	// ErrKeyRotation is a seig sample group: keys change within the track.
	ErrKeyRotation = errors.New("key rotation (seig sample group) is not supported")
	// ErrNoSampleEncryption is a protected track fragment without senc.
	ErrNoSampleEncryption = errors.New("protected samples without sample encryption box")
	// ErrMalformed is a box structure that cannot be parsed.
	ErrMalformed = errors.New("malformed fMP4")
)

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

// box is one ISO BMFF box inside a byte slice. Offsets are absolute.
type box struct {
	typ   string
	start int // first byte of the header
	body  int // first byte after the header (and after a uuid's usertype)
	end   int // one past the last byte
}

// piffSencUUID is the usertype of the PIFF 1.1 SampleEncryptionBox.
var piffSencUUID = [16]byte{0xA2, 0x39, 0x4F, 0x52, 0x5A, 0x9B, 0x4F, 0x14, 0xA2, 0x44, 0x6C, 0x42, 0x7C, 0x64, 0x8D, 0xF4}

// usertype returns a uuid box's 16-byte extended type.
func (b box) usertype(data []byte) [16]byte {
	var u [16]byte
	copy(u[:], data[b.body-16:b.body])
	return u
}

// boxes splits data[start:end] into boxes. Every box must fit exactly.
func boxes(data []byte, start, end int) ([]box, error) {
	var out []box
	for off := start; off < end; {
		if end-off < 8 {
			return nil, malformed("%d trailing bytes at offset %d", end-off, off)
		}
		size := uint64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		hdr := 8
		switch size {
		case 0:
			size = uint64(end - off)
		case 1:
			if end-off < 16 {
				return nil, malformed("truncated largesize header of %q", typ)
			}
			size = binary.BigEndian.Uint64(data[off+8:])
			hdr = 16
		}
		if typ == "uuid" {
			hdr += 16
		}
		if size < uint64(hdr) || size > uint64(end-off) {
			return nil, malformed("box %q at offset %d has size %d, %d bytes available", typ, off, size, end-off)
		}
		out = append(out, box{typ: typ, start: off, body: off + hdr, end: off + int(size)})
		off += int(size)
	}
	return out, nil
}

// children returns the boxes inside b, which starts skip bytes into its body
// (a full box's version and flags, a sample entry's fixed fields).
func children(data []byte, b box, skip int) ([]box, error) {
	if b.end-b.body < skip {
		return nil, malformed("box %q too short", b.typ)
	}
	return boxes(data, b.body+skip, b.end)
}

// child returns the first box of type typ in list.
func child(list []box, typ string) (box, bool) {
	for _, b := range list {
		if b.typ == typ {
			return b, true
		}
	}
	return box{}, false
}

// fullHeader returns a full box's version and flags.
func fullHeader(data []byte, b box) (version uint8, flags uint32, err error) {
	if b.end-b.body < 4 {
		return 0, 0, malformed("full box %q too short", b.typ)
	}
	v := binary.BigEndian.Uint32(data[b.body:])
	return uint8(v >> 24), v & 0xFFFFFF, nil
}

// reader reads big-endian fields from a box body, recording the first overrun.
type reader struct {
	data []byte
	off  int
	end  int
	err  error
}

func newReader(data []byte, from, to int) *reader { return &reader{data: data, off: from, end: to} }

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.end-r.off < n {
		r.err = malformed("field runs past end of box")
		return nil
	}
	b := r.data[r.off : r.off+n]
	r.off += n
	return b
}

func (r *reader) u8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.BigEndian.Uint16(b)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}
