package cenc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Key rotation (ISO/IEC 23001-7, 6): a track can move its samples to another
// KID, IV size or pattern with seig sample groups. An sgpd box of grouping
// type seig lists CencSampleEncryptionInformationGroupEntry entries, and an
// sbgp box of the same type maps runs of a fragment's samples to them. A
// sample mapped to no entry keeps the tenc defaults.

// localGroupBase is added to a group_description_index in a track fragment's
// sbgp that refers to the fragment's own sgpd; a smaller non-zero index
// refers to the init's (ISO/IEC 14496-12, 8.9.4).
const localGroupBase = 0x10000

// SeigEntry is one seig sample group entry: how the samples mapped to it are
// encrypted. Like TrackProtection it holds no key.
type SeigEntry struct {
	IsProtected     bool   `json:"is_protected"`
	PerSampleIVSize uint8  `json:"per_sample_iv_size"`
	KID             KID    `json:"kid"`
	ConstantIV      []byte `json:"constant_iv,omitempty"`
	CryptBlocks     uint8  `json:"crypt_blocks"`
	SkipBlocks      uint8  `json:"skip_blocks"`
}

// ErrMissingKey is a sample whose KID has no content key. The error
// DecryptSegment returns for it is a *MissingKeyError.
var ErrMissingKey = errors.New("no content key")

// MissingKeyError lists the KIDs a segment needs that the key lookup does
// not hold. It never holds a key.
type MissingKeyError struct {
	KIDs []KID
}

func (e *MissingKeyError) Error() string {
	ids := make([]string, len(e.KIDs))
	for i, k := range e.KIDs {
		ids[i] = k.String()
	}
	return fmt.Sprintf("%v for KID %s", ErrMissingKey, strings.Join(ids, ", "))
}

func (e *MissingKeyError) Unwrap() error { return ErrMissingKey }

// KeyLookup returns the content key for kid, and false when none is held.
type KeyLookup func(kid KID) ([16]byte, bool)

// Lookup is the KeyLookup of ks.
func (ks KeySet) Lookup(kid KID) ([16]byte, bool) {
	k, ok := ks[kid]
	return k, ok
}

// KIDs returns every KID the track's samples can need a key for: tenc's,
// and that of each protected seig entry of the init. tenc's is left out
// only when tenc marks samples clear and the init has seig entries, since
// then only those entries can protect a sample.
func (tp TrackProtection) KIDs() []KID {
	var out []KID
	if tp.IsProtected || len(tp.SeigGroups) == 0 {
		out = append(out, tp.KID)
	}
	for _, e := range tp.SeigGroups {
		if e.IsProtected && !slices.Contains(out, e.KID) {
			out = append(out, e.KID)
		}
	}
	return out
}

func unsupportedSeig(format string, a ...any) error {
	return fmt.Errorf("%w: seig sample group: %s", ErrUnsupportedScheme, fmt.Sprintf(format, a...))
}

// isSeig reports whether b is an sgpd or sbgp of grouping type seig.
func isSeig(data []byte, b box) bool {
	return (b.typ == "sgpd" || b.typ == "sbgp") && groupingType(data, b) == "seig"
}

// groupingType returns the grouping_type of an sgpd or sbgp.
func groupingType(data []byte, b box) string {
	if b.end-b.body < 8 {
		return ""
	}
	return string(data[b.body+4 : b.body+8])
}

// seigBox returns the only box of type typ (sgpd or sbgp) with grouping type
// seig in list. Two of them cannot be told apart for a sample.
func seigBox(data []byte, list []box, typ string) (box, bool, error) {
	var found box
	n := 0
	for _, b := range list {
		if b.typ == typ && isSeig(data, b) {
			found = b
			n++
		}
	}
	if n > 1 {
		return box{}, false, unsupportedSeig("%d %s boxes", n, typ)
	}
	return found, n == 1, nil
}

// readSeigSgpd reads the entries of an sgpd of grouping type seig, and its
// default_group_description_index (version 2; 0 for none). An entry that is
// malformed, or protects samples in a way scheme does not allow, makes the
// track unsupported.
func readSeigSgpd(data []byte, b box, scheme string) ([]SeigEntry, uint32, error) {
	v, _, err := fullHeader(data, b)
	if err != nil {
		return nil, 0, unsupportedSeig("%v", err)
	}
	r := newReader(data, b.body+8, b.end) // after version, flags and grouping_type
	var defaultLength, defaultIndex uint32
	if v == 1 {
		defaultLength = r.u32()
	}
	if v >= 2 {
		defaultIndex = r.u32()
	}
	count := r.u32()
	if r.err != nil {
		return nil, 0, unsupportedSeig("sgpd header truncated")
	}
	if uint64(count)*20 > uint64(r.end-r.off) {
		return nil, 0, unsupportedSeig("sgpd entry_count %d runs past the box", count)
	}
	out := make([]SeigEntry, 0, count)
	for i := range count {
		length := -1 // unknown: the entry delimits itself
		if v == 1 {
			length = int(defaultLength)
			if defaultLength == 0 {
				length = int(r.u32())
			}
		}
		start := r.off
		var e SeigEntry
		r.u8() // reserved
		pattern := r.u8()
		e.CryptBlocks, e.SkipBlocks = pattern>>4, pattern&0xF
		protected := r.u8()
		e.PerSampleIVSize = r.u8()
		copy(e.KID[:], r.take(16))
		if protected == 1 && e.PerSampleIVSize == 0 {
			n := r.u8()
			e.ConstantIV = slices.Clone(r.take(int(n)))
		}
		if r.err != nil {
			return nil, 0, unsupportedSeig("entry %d truncated", i+1)
		}
		if length >= 0 {
			if r.off-start > length {
				return nil, 0, unsupportedSeig("entry %d is longer than its %d bytes", i+1, length)
			}
			r.take(length - (r.off - start))
		}
		switch {
		case protected > 1:
			return nil, 0, unsupportedSeig("entry %d isProtected %d", i+1, protected)
		case protected == 0:
		case e.PerSampleIVSize == 8 || e.PerSampleIVSize == 16:
		case e.PerSampleIVSize == 0 && scheme == SchemeCBCS && (len(e.ConstantIV) == 8 || len(e.ConstantIV) == 16):
		default:
			return nil, 0, unsupportedSeig("entry %d IV size %d (constant %d) invalid for %s",
				i+1, e.PerSampleIVSize, len(e.ConstantIV), scheme)
		}
		e.IsProtected = protected == 1
		out = append(out, e)
	}
	if r.err != nil {
		return nil, 0, unsupportedSeig("sgpd truncated")
	}
	if defaultIndex > uint32(len(out)) {
		return nil, 0, unsupportedSeig("default_group_description_index %d of %d entries", defaultIndex, len(out))
	}
	return out, defaultIndex, nil
}

// sbgpRun is one entry of an sbgp: count samples mapped to index.
type sbgpRun struct {
	count, index uint32
}

func readSbgp(data []byte, b box) ([]sbgpRun, error) {
	v, _, err := fullHeader(data, b)
	if err != nil {
		return nil, unsupportedSeig("%v", err)
	}
	r := newReader(data, b.body+8, b.end)
	if v == 1 {
		r.u32() // grouping_type_parameter
	}
	count := r.u32()
	if r.err != nil || uint64(count)*8 > uint64(r.end-r.off) {
		return nil, unsupportedSeig("sbgp truncated")
	}
	out := make([]sbgpRun, count)
	for i := range out {
		out[i] = sbgpRun{count: r.u32(), index: r.u32()}
	}
	return out, r.err
}

// fragmentGroups works out the seig entry of each of a track fragment's n
// samples: nil for one that keeps the tenc defaults. retype lists the seig
// boxes of the fragment.
func fragmentGroups(data []byte, kids []box, tp TrackProtection, n int) (groups []*SeigEntry, retype []int, err error) {
	sgpd, hasSgpd, err := seigBox(data, kids, "sgpd")
	if err != nil {
		return nil, nil, err
	}
	sbgp, hasSbgp, err := seigBox(data, kids, "sbgp")
	if err != nil {
		return nil, nil, err
	}
	var local []SeigEntry
	var localDefault uint32
	if hasSgpd {
		retype = append(retype, sgpd.start)
		if local, localDefault, err = readSeigSgpd(data, sgpd, tp.Scheme); err != nil {
			return nil, nil, err
		}
	}

	// A sample no sbgp entry maps takes the default of the fragment's sgpd,
	// else that of the init's, else tenc's.
	var unmapped *SeigEntry
	switch {
	case localDefault != 0:
		unmapped = &local[localDefault-1]
	case tp.SeigDefault != 0:
		if int(tp.SeigDefault) > len(tp.SeigGroups) {
			return nil, nil, unsupportedSeig("init default index %d of %d entries", tp.SeigDefault, len(tp.SeigGroups))
		}
		unmapped = &tp.SeigGroups[tp.SeigDefault-1]
	}
	groups = make([]*SeigEntry, n)
	for i := range groups {
		groups[i] = unmapped
	}
	if !hasSbgp {
		return groups, retype, nil
	}
	retype = append(retype, sbgp.start)
	runs, err := readSbgp(data, sbgp)
	if err != nil {
		return nil, nil, err
	}
	pos := 0
	for _, run := range runs {
		if uint64(run.count) > uint64(n-pos) {
			return nil, nil, malformed("sbgp maps more samples than the fragment's %d", n)
		}
		var e *SeigEntry
		switch idx := run.index; {
		case idx == 0: // in no group: tenc's defaults
		case idx > localGroupBase:
			i := idx - localGroupBase
			if uint64(i) > uint64(len(local)) {
				return nil, nil, unsupportedSeig("sbgp index %#x, fragment has %d entries", idx, len(local))
			}
			e = &local[i-1]
		default:
			if uint64(idx) > uint64(len(tp.SeigGroups)) {
				return nil, nil, unsupportedSeig("sbgp index %d, init has %d entries", idx, len(tp.SeigGroups))
			}
			e = &tp.SeigGroups[idx-1]
		}
		for range run.count {
			groups[pos] = e
			pos++
		}
	}
	return groups, retype, nil
}
