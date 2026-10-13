package cenc

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"slices"
)

// Retyped boxes become free boxes of the same size, so nothing after them
// moves. See ADR 0010.
const freeType = "free"

type subsample struct {
	clear, protected int
}

// sample is one encrypted sample: where its bytes are and how to decrypt them.
type sample struct {
	off, size int
	iv        []byte
	subs      []subsample // nil: the whole sample is protected
}

// trafJob is what decrypting one track fragment takes. It is built in full
// before any byte changes, so a malformed segment is never half-decrypted.
type trafJob struct {
	samples []sample
	retype  []int // offsets of box headers whose type becomes free
	decrypt bool
}

// DecryptSegment decrypts, in place, every fragment of track tp.TrackID in a
// media segment. Sample bytes are decrypted where they lie; senc, saiz, saio
// and pssh boxes are retyped to free. The segment's length and the size and
// position of every box stay the same.
//
// A fragment whose sample description is not one of tp.ProtectedEntries is a
// clear lead and is left untouched. A wrong key is not detected: the samples
// decrypt to different bytes and no error is returned.
func DecryptSegment(seg []byte, tp TrackProtection, key [16]byte) error {
	top, err := boxes(seg, 0, len(seg))
	if err != nil {
		return err
	}
	var mdats [][2]int
	for _, b := range top {
		if b.typ == "mdat" {
			mdats = append(mdats, [2]int{b.body, b.end})
		}
	}

	var jobs []trafJob
	matched := false
	for _, moof := range top {
		if moof.typ != "moof" {
			continue
		}
		kids, err := children(seg, moof, 0)
		if err != nil {
			return err
		}
		var moofPssh []int
		protectedMoof := false
		prevEnd := -1 // end of the previous traf's data, for an implicit base
		for i, traf := range kids {
			if traf.typ == "pssh" {
				moofPssh = append(moofPssh, traf.start)
			}
			if traf.typ != "traf" {
				continue
			}
			job, end, ours, err := planTraf(seg, moof, traf, i == firstTraf(kids), prevEnd, tp, mdats)
			if err != nil {
				return err
			}
			prevEnd = end
			if !ours {
				continue
			}
			matched = true
			if len(job.retype) > 0 {
				protectedMoof = true
				jobs = append(jobs, job)
			}
		}
		if protectedMoof {
			jobs = append(jobs, trafJob{retype: moofPssh})
		}
	}
	if !matched {
		return malformed("no track fragment for track %d", tp.TrackID)
	}

	var block cipher.Block
	for _, job := range jobs {
		if job.decrypt && block == nil {
			if block, err = aes.NewCipher(key[:]); err != nil {
				return err
			}
		}
		if job.decrypt {
			for _, s := range job.samples {
				buf := seg[s.off : s.off+s.size]
				if tp.Scheme == SchemeCBCS {
					decryptCBCS(block, s, buf, int(tp.CryptBlocks), int(tp.SkipBlocks))
				} else {
					decryptCENC(block, s, buf)
				}
			}
		}
		for _, off := range job.retype {
			copy(seg[off+4:off+8], freeType)
		}
	}
	return nil
}

// firstTraf returns the index of the first traf in a moof's children.
func firstTraf(kids []box) int {
	for i, b := range kids {
		if b.typ == "traf" {
			return i
		}
	}
	return -1
}

type tfhd struct {
	trackID     uint32
	flags       uint32
	base        uint64
	sdi         uint32
	defaultSize uint32
}

func readTfhd(data []byte, b box) (tfhd, error) {
	var h tfhd
	_, flags, err := fullHeader(data, b)
	if err != nil {
		return h, err
	}
	h.flags = flags
	r := newReader(data, b.body+4, b.end)
	h.trackID = r.u32()
	if flags&0x1 != 0 {
		h.base = r.u64()
	}
	if flags&0x2 != 0 {
		h.sdi = r.u32()
	}
	if flags&0x8 != 0 {
		r.u32()
	}
	if flags&0x10 != 0 {
		h.defaultSize = r.u32()
	}
	return h, r.err
}

// planTraf works out where one traf's samples are and how they are
// encrypted. end is where its data ends, for the next traf's implicit base.
// ours reports whether the traf belongs to tp's track.
func planTraf(seg []byte, moof, traf box, first bool, prevEnd int, tp TrackProtection, mdats [][2]int) (job trafJob, end int, ours bool, err error) {
	kids, err := children(seg, traf, 0)
	if err != nil {
		return job, 0, false, err
	}
	hb, ok := child(kids, "tfhd")
	if !ok {
		return job, 0, false, malformed("traf without tfhd")
	}
	h, err := readTfhd(seg, hb)
	if err != nil {
		return job, 0, false, err
	}
	ours = h.trackID == tp.TrackID

	// Locate the samples. Base offsets are taken relative to the segment.
	base := -1
	switch {
	case h.flags&0x1 != 0:
		if h.base > uint64(len(seg)) {
			return job, 0, ours, malformed("base_data_offset %d past segment end", h.base)
		}
		base = int(h.base)
	case h.flags&0x20000 != 0 || first:
		base = moof.start
	default:
		base = prevEnd
	}
	defaultSize := tp.DefaultSampleSize
	if h.flags&0x10 != 0 {
		defaultSize = h.defaultSize
	}
	var samples []sample
	pos := base
	for _, b := range kids {
		if b.typ != "trun" {
			continue
		}
		if pos, err = readTrun(seg, b, base, pos, defaultSize, &samples); err != nil {
			return job, 0, ours, err
		}
	}
	end = pos
	if !ours {
		return job, end, false, nil
	}
	if base < 0 && len(samples) > 0 {
		return job, 0, true, malformed("cannot locate the data of track %d", tp.TrackID)
	}

	if hasSeig(seg, kids) {
		return job, 0, true, fmt.Errorf("%w: track %d fragment", ErrKeyRotation, tp.TrackID)
	}
	sdi := tp.DefaultSampleDescriptionIndex
	if h.flags&0x2 != 0 {
		sdi = h.sdi
	}
	if sdi == 0 {
		sdi = 1
	}
	if !slices.Contains(tp.ProtectedEntries, sdi) {
		return job, end, true, nil // clear lead
	}

	senc, piff, found := findSenc(seg, kids)
	if !found {
		if tp.IsProtected && len(samples) > 0 {
			return job, 0, true, fmt.Errorf("%w: track %d", ErrNoSampleEncryption, tp.TrackID)
		}
		return job, end, true, nil
	}
	if err := readSenc(seg, senc, piff, tp, samples); err != nil {
		return job, 0, true, err
	}
	for _, s := range samples {
		if !inside(mdats, s.off, s.size) {
			return job, 0, true, malformed("sample at %d (%d bytes) is outside mdat", s.off, s.size)
		}
	}

	job.samples = samples
	job.decrypt = tp.IsProtected
	job.retype = []int{senc.start}
	for _, b := range kids {
		if b.typ == "saiz" || b.typ == "saio" {
			job.retype = append(job.retype, b.start)
		}
	}
	return job, end, true, nil
}

// readTrun appends a trun's samples. pos is where the previous trun's data
// ended; it is used when this trun has no data_offset.
func readTrun(data []byte, b box, base, pos int, defaultSize uint32, out *[]sample) (int, error) {
	_, flags, err := fullHeader(data, b)
	if err != nil {
		return 0, err
	}
	r := newReader(data, b.body+4, b.end)
	count := r.u32()
	if flags&0x1 != 0 {
		if base < 0 {
			return 0, malformed("trun data_offset without a base")
		}
		pos = base + int(int32(r.u32()))
	}
	if flags&0x4 != 0 {
		r.u32()
	}
	if r.err != nil {
		return 0, r.err
	}
	per := 0
	for _, f := range []uint32{0x100, 0x200, 0x400, 0x800} {
		if flags&f != 0 {
			per += 4
		}
	}
	if uint64(count)*uint64(per) > uint64(r.end-r.off) {
		return 0, malformed("trun sample count %d runs past the box", count)
	}
	for range count {
		if flags&0x100 != 0 {
			r.u32()
		}
		size := defaultSize
		if flags&0x200 != 0 {
			size = r.u32()
		}
		if flags&0x400 != 0 {
			r.u32()
		}
		if flags&0x800 != 0 {
			r.u32()
		}
		if pos < 0 || uint64(size) > uint64(len(data)) || pos > len(data)-int(size) {
			return 0, malformed("sample of %d bytes at %d runs past the segment", size, pos)
		}
		*out = append(*out, sample{off: pos, size: int(size)})
		pos += int(size)
	}
	return pos, r.err
}

func findSenc(data []byte, kids []box) (b box, piff, ok bool) {
	for _, k := range kids {
		if k.typ == "senc" {
			return k, false, true
		}
		if k.typ == "uuid" && k.usertype(data) == piffSencUUID {
			return k, true, true
		}
	}
	return box{}, false, false
}

// readSenc fills each sample's IV and subsamples from a senc (or PIFF senc).
func readSenc(data []byte, b box, piff bool, tp TrackProtection, samples []sample) error {
	_, flags, err := fullHeader(data, b)
	if err != nil {
		return err
	}
	r := newReader(data, b.body+4, b.end)
	ivSize := int(tp.PerSampleIVSize)
	if piff && flags&0x1 != 0 {
		r.take(3) // AlgorithmID
		ivSize = int(r.u8())
		r.take(16) // KID
	}
	count := r.u32()
	if r.err != nil {
		return r.err
	}
	if int(count) != len(samples) {
		return malformed("senc has %d samples, trun has %d", count, len(samples))
	}
	for i := range samples {
		s := &samples[i]
		if ivSize > 0 {
			s.iv = r.take(ivSize)
		} else {
			s.iv = tp.ConstantIV
		}
		if flags&0x2 == 0 {
			continue
		}
		n := int(r.u16())
		total := 0
		for range n {
			c, p := int(r.u16()), int(r.u32())
			s.subs = append(s.subs, subsample{clear: c, protected: p})
			total += c + p
		}
		if r.err != nil {
			return malformed("senc truncated at sample %d", i)
		}
		if total != s.size {
			return malformed("sample %d subsamples cover %d of %d bytes", i, total, s.size)
		}
	}
	if r.err != nil {
		return malformed("senc truncated")
	}
	if r.off != r.end {
		return malformed("senc has %d bytes left over: IV size %d does not match", r.end-r.off, ivSize)
	}
	return nil
}

func inside(ranges [][2]int, off, size int) bool {
	for _, m := range ranges {
		if off >= m[0] && off+size <= m[1] {
			return true
		}
	}
	return false
}

// iv16 widens an 8-byte IV with zeros on the right, as both schemes specify.
func iv16(iv []byte) []byte {
	out := make([]byte, 16)
	copy(out, iv)
	return out
}

// protectedRanges calls fn with each protected byte range of a sample.
func protectedRanges(s sample, buf []byte, fn func([]byte)) {
	if s.subs == nil {
		fn(buf)
		return
	}
	pos := 0
	for _, sub := range s.subs {
		pos += sub.clear
		fn(buf[pos : pos+sub.protected])
		pos += sub.protected
	}
}

// decryptCENC runs one AES-CTR keystream through the protected bytes of a
// sample: the counter carries on from one subsample to the next.
func decryptCENC(block cipher.Block, s sample, buf []byte) {
	ctr := cipher.NewCTR(block, iv16(s.iv))
	protectedRanges(s, buf, func(p []byte) { ctr.XORKeyStream(p, p) })
}

// decryptCBCS decrypts each protected range with AES-CBC, restarting from the
// sample's IV for every subsample. Within a range the pattern decrypts crypt
// blocks and skips skip blocks; a trailing partial block is clear. A 0:0
// pattern means every whole block is encrypted.
func decryptCBCS(block cipher.Block, s sample, buf []byte, crypt, skip int) {
	if crypt == 0 && skip == 0 {
		crypt = 1
	}
	iv := iv16(s.iv)
	protectedRanges(s, buf, func(p []byte) {
		cbc := cipher.NewCBCDecrypter(block, iv)
		for pos := 0; len(p)-pos >= aes.BlockSize; pos += skip * aes.BlockSize {
			n := min(crypt*aes.BlockSize, (len(p)-pos)/aes.BlockSize*aes.BlockSize)
			cbc.CryptBlocks(p[pos:pos+n], p[pos:pos+n])
			pos += n
		}
	})
}
