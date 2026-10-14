// Package cenctest builds key-rotation test segments that no packager at hand
// produces: Bento4 1.6 cannot write seig sample groups. It encrypts a clear
// fragmented segment with the cenc scheme (AES-CTR over whole samples) using
// crypto/aes directly, so a test of internal/cenc does not check that package
// against itself.
package cenctest

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"slices"
)

// Group is how a run of samples is encrypted: tenc's defaults, or one seig
// sample group entry.
type Group struct {
	Protected bool
	// IVSize is the per-sample IV size, 8 or 16, of a protected group.
	IVSize uint8
	KID    [16]byte
}

// LocalBase is added to an sbgp index that refers to the fragment's own sgpd.
const LocalBase = 0x10000

// Rotation says how Rotate encrypts the samples of a segment's one track
// fragment.
type Rotation struct {
	// Default is tenc's protection, for samples in no group (index 0).
	Default Group
	// Init holds the init's seig entries, indexes 1 to LocalBase.
	Init []Group
	// Local holds the entries Rotate writes into the fragment's sgpd,
	// indexes LocalBase+1 on. No sgpd is written when it is empty.
	Local []Group
	// Index is each sample's group_description_index; nil writes no sbgp.
	Index []uint32
	Keys  map[[16]byte][16]byte
}

// Rotate returns clear, a segment with one moof holding one traf whose trun
// lists sample sizes and a data offset, encrypted as rot says: the samples
// are encrypted in place, and a senc, an sbgp and an sgpd are appended to
// the traf.
func Rotate(clear []byte, rot Rotation) ([]byte, error) {
	seg := slices.Clone(clear)
	moof, ok := find(seg, 0, len(seg), "moof")
	if !ok {
		return nil, fmt.Errorf("no moof")
	}
	traf, ok := find(seg, moof.body, moof.end, "traf")
	if !ok {
		return nil, fmt.Errorf("no traf")
	}
	trun, ok := find(seg, traf.body, traf.end, "trun")
	if !ok {
		return nil, fmt.Errorf("no trun")
	}
	flags := binary.BigEndian.Uint32(seg[trun.body:]) & 0xFFFFFF
	if flags&0x1 == 0 || flags&0x200 == 0 {
		return nil, fmt.Errorf("trun flags %#x: want a data offset and sample sizes", flags)
	}
	count := int(binary.BigEndian.Uint32(seg[trun.body+4:]))
	pos := moof.start + int(int32(binary.BigEndian.Uint32(seg[trun.body+8:])))
	field := trun.body + 12
	if flags&0x4 != 0 {
		field += 4
	}
	if rot.Index != nil && len(rot.Index) != count {
		return nil, fmt.Errorf("%d indexes for %d samples", len(rot.Index), count)
	}

	senc := binary.BigEndian.AppendUint32(nil, 0) // version 0, no subsamples
	senc = binary.BigEndian.AppendUint32(senc, uint32(count))
	for i := range count {
		for _, f := range []uint32{0x100, 0x200, 0x400, 0x800} {
			if flags&f == 0 {
				continue
			}
			v := binary.BigEndian.Uint32(seg[field:])
			field += 4
			if f != 0x200 {
				continue
			}
			g, err := rot.group(i)
			if err != nil {
				return nil, err
			}
			if g.Protected {
				iv := make([]byte, g.IVSize)
				for j := range iv {
					iv[j] = byte(i*7 + j + 1)
				}
				senc = append(senc, iv...)
				key, ok := rot.Keys[g.KID]
				if !ok {
					return nil, fmt.Errorf("no key for KID %x", g.KID)
				}
				block, err := aes.NewCipher(key[:])
				if err != nil {
					return nil, err
				}
				ctr := make([]byte, 16)
				copy(ctr, iv)
				buf := seg[pos : pos+int(v)]
				cipher.NewCTR(block, ctr).XORKeyStream(buf, buf)
			}
			pos += int(v)
		}
	}

	add := Box("senc", senc)
	if rot.Index != nil {
		add = append(add, Sbgp(rot.Index)...)
	}
	if len(rot.Local) > 0 {
		add = append(add, Sgpd(rot.Local)...)
	}
	seg, err := Insert(seg, []string{"moof", "traf"}, add)
	if err != nil {
		return nil, err
	}
	// The samples moved len(add) further from the moof that data_offset
	// counts from. The trun itself precedes the insertion, so it stays put.
	off := trun.body + 8
	binary.BigEndian.PutUint32(seg[off:], binary.BigEndian.Uint32(seg[off:])+uint32(len(add)))
	return seg, nil
}

func (rot Rotation) group(i int) (Group, error) {
	if rot.Index == nil {
		return rot.Default, nil
	}
	switch idx := rot.Index[i]; {
	case idx == 0:
		return rot.Default, nil
	case idx > LocalBase && int(idx-LocalBase) <= len(rot.Local):
		return rot.Local[idx-LocalBase-1], nil
	case idx <= LocalBase && int(idx) <= len(rot.Init):
		return rot.Init[idx-1], nil
	}
	return Group{}, fmt.Errorf("sample %d: no group %#x", i, rot.Index[i])
}

// Sgpd returns an sgpd box of grouping type seig (version 1, 20-byte
// entries) holding groups.
func Sgpd(groups []Group) []byte {
	body := []byte{1, 0, 0, 0}
	body = append(body, "seig"...)
	body = binary.BigEndian.AppendUint32(body, 20)
	body = binary.BigEndian.AppendUint32(body, uint32(len(groups)))
	for _, g := range groups {
		protected := byte(0)
		if g.Protected {
			protected = 1
		}
		body = append(body, 0, 0, protected, g.IVSize)
		body = append(body, g.KID[:]...)
	}
	return Box("sgpd", body)
}

// Sbgp returns an sbgp box of grouping type seig mapping sample i to
// index[i], in runs.
func Sbgp(index []uint32) []byte {
	type run struct{ n, idx uint32 }
	var runs []run
	for _, idx := range index {
		if len(runs) > 0 && runs[len(runs)-1].idx == idx {
			runs[len(runs)-1].n++
			continue
		}
		runs = append(runs, run{1, idx})
	}
	body := []byte{0, 0, 0, 0}
	body = append(body, "seig"...)
	body = binary.BigEndian.AppendUint32(body, uint32(len(runs)))
	for _, r := range runs {
		body = binary.BigEndian.AppendUint32(body, r.n)
		body = binary.BigEndian.AppendUint32(body, r.idx)
	}
	return Box("sbgp", body)
}

// Box returns a box of type typ holding body.
func Box(typ string, body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(out, typ...), body...)
}

// Insert returns data with add appended to the end of the container that
// path names (each element the first box of that type inside the last),
// growing every container on the path. Offsets that point past the
// insertion are not fixed.
func Insert(data []byte, path []string, add []byte) ([]byte, error) {
	start, end := 0, len(data)
	var chain []span
	for _, typ := range path {
		b, ok := find(data, start, end, typ)
		if !ok {
			return nil, fmt.Errorf("no %s in %v", typ, path)
		}
		chain = append(chain, b)
		start, end = b.body, b.end
	}
	at := chain[len(chain)-1].end
	out := slices.Concat(data[:at], add, data[at:])
	for _, b := range chain {
		binary.BigEndian.PutUint32(out[b.start:], binary.BigEndian.Uint32(out[b.start:])+uint32(len(add)))
	}
	return out, nil
}

type span struct{ start, body, end int }

// find returns the first box of type typ in data[start:end], descending
// into nothing. Only 32-bit box sizes are handled.
func find(data []byte, start, end int, typ string) (span, bool) {
	for off := start; off+8 <= end; {
		size := int(binary.BigEndian.Uint32(data[off:]))
		if size < 8 || off+size > end {
			return span{}, false
		}
		if string(data[off+4:off+8]) == typ {
			return span{start: off, body: off + 8, end: off + size}, true
		}
		off += size
	}
	return span{}, false
}
