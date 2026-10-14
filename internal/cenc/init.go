package cenc

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// Schemes this package decrypts.
const (
	SchemeCENC = "cenc"
	SchemeCBCS = "cbcs"
)

// TrackProtection is how one track of an init segment is encrypted, as read
// from its tenc, its seig sample group and trex. It holds no key, so it can be persisted.
type TrackProtection struct {
	TrackID uint32 `json:"track_id"`
	// Scheme is the schm scheme type: "cenc" or "cbcs".
	Scheme string `json:"scheme"`
	// OriginalFormat is the frma original format, e.g. "avc1" or "mp4a".
	OriginalFormat string `json:"original_format"`
	KID            KID    `json:"kid"`
	// IsProtected is tenc's default_isProtected.
	IsProtected bool `json:"is_protected"`
	// PerSampleIVSize is 0, 8 or 16. When 0, ConstantIV is used.
	PerSampleIVSize uint8  `json:"per_sample_iv_size"`
	ConstantIV      []byte `json:"constant_iv,omitempty"`
	// CryptBlocks and SkipBlocks are the cbcs pattern; 0:0 means every
	// whole block of a protected range is encrypted.
	CryptBlocks uint8 `json:"crypt_blocks"`
	SkipBlocks  uint8 `json:"skip_blocks"`
	// ProtectedEntries lists the 1-based stsd indexes of the encrypted
	// sample entries. A fragment that uses any other entry is clear (a clear
	// lead) and is left alone.
	ProtectedEntries []uint32 `json:"protected_entries"`
	// DefaultSampleDescriptionIndex and DefaultSampleSize come from trex and
	// apply to fragments whose tfhd does not override them.
	DefaultSampleDescriptionIndex uint32 `json:"default_sample_description_index"`
	DefaultSampleSize             uint32 `json:"default_sample_size"`
	// SeigGroups are the entries of the init's seig sample group (key
	// rotation), which a fragment's sbgp refers to by 1-based index.
	// SeigDefault is the 1-based entry for samples no sbgp maps, 0 for none.
	// A decrypt.json written before key rotation was supported has neither.
	SeigGroups  []SeigEntry `json:"seig_groups,omitempty"`
	SeigDefault uint32      `json:"seig_default,omitempty"`
}

// sampleEntryFixed is the length of a sample entry's fixed fields, which sit
// between its header and its child boxes.
func sampleEntryFixed(data []byte, e box) (int, error) {
	switch e.typ {
	case "encv":
		return 78, nil
	case "enca":
		// QuickTime sound description versions 1 and 2 extend the entry.
		if e.end-e.body < 10 {
			return 0, malformed("enca too short")
		}
		switch binary.BigEndian.Uint16(data[e.body+8:]) {
		case 0:
			return 28, nil
		case 1:
			return 44, nil
		case 2:
			return 64, nil
		}
		return 0, malformed("enca has an unknown sound description version")
	}
	return 0, fmt.Errorf("%w: encrypted sample entry %q", ErrUnsupportedScheme, e.typ)
}

func isEncryptedEntry(typ string) bool {
	return typ == "encv" || typ == "enca" || typ == "encs" || typ == "enct" || typ == "encm"
}

// trakInfo is what one trak contributes.
type trakInfo struct {
	trackID uint32
	stsd    box   // the stsd box
	entries []box // stsd's sample entries
	stbl    []box // stbl's children
}

func readTrak(data []byte, trak box) (trakInfo, error) {
	var ti trakInfo
	kids, err := children(data, trak, 0)
	if err != nil {
		return ti, err
	}
	tkhd, ok := child(kids, "tkhd")
	if !ok {
		return ti, malformed("trak without tkhd")
	}
	v, _, err := fullHeader(data, tkhd)
	if err != nil {
		return ti, err
	}
	r := newReader(data, tkhd.body+4, tkhd.end)
	if v == 1 {
		r.take(16)
	} else {
		r.take(8)
	}
	ti.trackID = r.u32()
	if r.err != nil {
		return ti, r.err
	}

	cur := kids
	for _, typ := range []string{"mdia", "minf", "stbl"} {
		b, ok := child(cur, typ)
		if !ok {
			return ti, malformed("trak without %s", typ)
		}
		if cur, err = children(data, b, 0); err != nil {
			return ti, err
		}
	}
	ti.stbl = cur
	stsd, ok := child(cur, "stsd")
	if !ok {
		return ti, malformed("stbl without stsd")
	}
	ti.stsd = stsd
	if ti.entries, err = children(data, stsd, 8); err != nil {
		return ti, err
	}
	if n := binary.BigEndian.Uint32(data[stsd.body+4:]); int(n) != len(ti.entries) {
		return ti, malformed("stsd entry_count %d, %d entries", n, len(ti.entries))
	}
	return ti, nil
}

// readSinf reads one encrypted sample entry's protection.
func readSinf(data []byte, e box) (TrackProtection, error) {
	var tp TrackProtection
	fixed, err := sampleEntryFixed(data, e)
	if err != nil {
		return tp, err
	}
	kids, err := children(data, e, fixed)
	if err != nil {
		return tp, err
	}
	sinf, ok := child(kids, "sinf")
	if !ok {
		return tp, malformed("%s without sinf", e.typ)
	}
	sk, err := children(data, sinf, 0)
	if err != nil {
		return tp, err
	}
	frma, ok := child(sk, "frma")
	if !ok || frma.end-frma.body != 4 {
		return tp, malformed("sinf without frma")
	}
	tp.OriginalFormat = string(data[frma.body:frma.end])
	schm, ok := child(sk, "schm")
	if !ok || schm.end-schm.body < 12 {
		return tp, malformed("sinf without schm")
	}
	tp.Scheme = string(data[schm.body+4 : schm.body+8])
	if tp.Scheme != SchemeCENC && tp.Scheme != SchemeCBCS {
		return tp, fmt.Errorf("%w: %q", ErrUnsupportedScheme, tp.Scheme)
	}
	schi, ok := child(sk, "schi")
	if !ok {
		return tp, malformed("sinf without schi")
	}
	sc, err := children(data, schi, 0)
	if err != nil {
		return tp, err
	}
	tenc, ok := child(sc, "tenc")
	if !ok {
		return tp, malformed("schi without tenc")
	}
	v, _, err := fullHeader(data, tenc)
	if err != nil {
		return tp, err
	}
	r := newReader(data, tenc.body+4, tenc.end)
	r.u8() // reserved
	pattern := r.u8()
	if v >= 1 {
		tp.CryptBlocks, tp.SkipBlocks = pattern>>4, pattern&0xF
	}
	tp.IsProtected = r.u8() == 1
	tp.PerSampleIVSize = r.u8()
	copy(tp.KID[:], r.take(16))
	if tp.IsProtected && tp.PerSampleIVSize == 0 {
		n := r.u8()
		tp.ConstantIV = slices.Clone(r.take(int(n)))
	}
	if r.err != nil {
		return tp, r.err
	}
	if tp.IsProtected {
		switch {
		case tp.PerSampleIVSize == 8 || tp.PerSampleIVSize == 16:
		case tp.PerSampleIVSize == 0 && tp.Scheme == SchemeCBCS && (len(tp.ConstantIV) == 8 || len(tp.ConstantIV) == 16):
		default:
			return tp, malformed("tenc IV size %d (constant %d) invalid for %s", tp.PerSampleIVSize, len(tp.ConstantIV), tp.Scheme)
		}
	}
	return tp, nil
}

func sameProtection(a, b TrackProtection) bool {
	return a.Scheme == b.Scheme && a.KID == b.KID && a.IsProtected == b.IsProtected &&
		a.PerSampleIVSize == b.PerSampleIVSize && slices.Equal(a.ConstantIV, b.ConstantIV) &&
		a.CryptBlocks == b.CryptBlocks && a.SkipBlocks == b.SkipBlocks && a.OriginalFormat == b.OriginalFormat
}

func topLevel(init []byte) ([]box, box, error) {
	top, err := boxes(init, 0, len(init))
	if err != nil {
		return nil, box{}, err
	}
	moov, ok := child(top, "moov")
	if !ok {
		return nil, box{}, malformed("init without moov")
	}
	return top, moov, nil
}

// ReadInitProtection returns the protection of every encrypted track in an
// init segment, in trak order. A clear init returns nil, nil.
func ReadInitProtection(init []byte) ([]TrackProtection, error) {
	_, moov, err := topLevel(init)
	if err != nil {
		return nil, err
	}
	mk, err := children(init, moov, 0)
	if err != nil {
		return nil, err
	}
	trex := map[uint32][2]uint32{} // track id -> default sdi, default size
	if mvex, ok := child(mk, "mvex"); ok {
		xk, err := children(init, mvex, 0)
		if err != nil {
			return nil, err
		}
		for _, b := range xk {
			if b.typ != "trex" {
				continue
			}
			r := newReader(init, b.body+4, b.end)
			id, sdi := r.u32(), r.u32()
			r.u32() // default_sample_duration
			size := r.u32()
			if r.err != nil {
				return nil, r.err
			}
			trex[id] = [2]uint32{sdi, size}
		}
	}

	var out []TrackProtection
	for _, trak := range mk {
		if trak.typ != "trak" {
			continue
		}
		ti, err := readTrak(init, trak)
		if err != nil {
			return nil, err
		}
		var tp *TrackProtection
		for i, e := range ti.entries {
			if !isEncryptedEntry(e.typ) {
				continue
			}
			p, err := readSinf(init, e)
			if err != nil {
				return nil, fmt.Errorf("track %d: %w", ti.trackID, err)
			}
			if tp == nil {
				tp = &p
			} else if !sameProtection(*tp, p) {
				return nil, fmt.Errorf("%w: track %d has sample entries with different protection", ErrUnsupportedScheme, ti.trackID)
			}
			tp.ProtectedEntries = append(tp.ProtectedEntries, uint32(i+1))
		}
		if tp == nil {
			continue
		}
		sgpd, ok, err := seigBox(init, ti.stbl, "sgpd")
		if err == nil && ok {
			tp.SeigGroups, tp.SeigDefault, err = readSeigSgpd(init, sgpd, tp.Scheme)
		}
		if err != nil {
			return nil, fmt.Errorf("track %d: %w", ti.trackID, err)
		}
		tp.TrackID = ti.trackID
		tp.DefaultSampleDescriptionIndex, tp.DefaultSampleSize = 1, 0
		if d, ok := trex[ti.trackID]; ok {
			tp.DefaultSampleDescriptionIndex, tp.DefaultSampleSize = d[0], d[1]
		}
		out = append(out, *tp)
	}
	return out, nil
}

// ClearInit rewrites an encrypted init segment as a clear one: every encv or
// enca sample entry takes its frma original format and loses its sinf, and
// pssh boxes and seig sample groups (sgpd, sbgp) are dropped. The stsd keeps its entry count and order. A clear
// init comes back unchanged (as a copy).
func ClearInit(init []byte) ([]byte, error) {
	tps, err := ReadInitProtection(init)
	if err != nil {
		return nil, err
	}
	if tps == nil {
		return slices.Clone(init), nil
	}
	top, _, err := topLevel(init)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(init))
	for _, b := range top {
		switch b.typ {
		case "pssh":
		case "moov":
			if out, err = rewriteContainer(out, init, b, "moov"); err != nil {
				return nil, err
			}
		default:
			out = append(out, init[b.start:b.end]...)
		}
	}
	return out, nil
}

// clearPath is the chain of containers ClearInit descends to reach stsd.
var clearPath = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true}

// rewriteContainer appends container b, with its children rewritten, to out.
func rewriteContainer(out, data []byte, b box, typ string) ([]byte, error) {
	kids, err := children(data, b, 0)
	if err != nil {
		return nil, err
	}
	var body []byte
	for _, c := range kids {
		switch {
		case typ == "moov" && c.typ == "pssh":
		case typ == "stbl" && isSeig(data, c):
		case clearPath[c.typ]:
			if body, err = rewriteContainer(body, data, c, c.typ); err != nil {
				return nil, err
			}
		case c.typ == "stsd":
			if body, err = rewriteStsd(body, data, c); err != nil {
				return nil, err
			}
		default:
			body = append(body, data[c.start:c.end]...)
		}
	}
	return appendBox(out, typ, body), nil
}

func rewriteStsd(out, data []byte, stsd box) ([]byte, error) {
	entries, err := children(data, stsd, 8)
	if err != nil {
		return nil, err
	}
	body := append([]byte(nil), data[stsd.body:stsd.body+8]...)
	for _, e := range entries {
		if !isEncryptedEntry(e.typ) {
			body = append(body, data[e.start:e.end]...)
			continue
		}
		fixed, err := sampleEntryFixed(data, e)
		if err != nil {
			return nil, err
		}
		kids, err := children(data, e, fixed)
		if err != nil {
			return nil, err
		}
		format := ""
		entry := append([]byte(nil), data[e.body:e.body+fixed]...)
		for _, c := range kids {
			if c.typ != "sinf" {
				entry = append(entry, data[c.start:c.end]...)
				continue
			}
			sk, err := children(data, c, 0)
			if err != nil {
				return nil, err
			}
			if frma, ok := child(sk, "frma"); ok && frma.end-frma.body == 4 {
				format = string(data[frma.body:frma.end])
			}
		}
		if format == "" {
			return nil, malformed("%s without frma", e.typ)
		}
		body = appendBox(body, format, entry)
	}
	return appendBox(out, "stsd", body), nil
}

// appendBox appends a box header for body, then body.
func appendBox(out []byte, typ string, body []byte) []byte {
	if size := uint64(len(body)) + 8; size <= 0xFFFFFFFF {
		out = binary.BigEndian.AppendUint32(out, uint32(size))
		out = append(out, typ...)
	} else {
		out = binary.BigEndian.AppendUint32(out, 1)
		out = append(out, typ...)
		out = binary.BigEndian.AppendUint64(out, size+8)
	}
	return append(out, body...)
}
