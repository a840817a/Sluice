// Package validate provides ISO BMFF structure checks for DASH segments.
package validate

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// SegmentInfo holds the key metadata extracted from a validated media segment.
type SegmentInfo struct {
	// BaseMediaDecodeTime from the tfdt box (in the segment's timescale units).
	BaseMediaDecodeTime uint64
	// HasMoof is true when the segment contains a moof box (expected for fMP4).
	HasMoof bool
	// HasMdat is true when the segment contains a mdat box.
	HasMdat bool
}

// ValidateSegment checks the BMFF structure of a media segment (fMP4 / CMAF).
// It verifies:
//   - moof box is present
//   - mdat box is present
//   - tfdt box is present and returns BaseMediaDecodeTime
//
// Returns an error if any check fails, otherwise the extracted SegmentInfo.
func ValidateSegment(data []byte) (SegmentInfo, error) {
	r := bytes.NewReader(data)

	var info SegmentInfo

	// Extract moof
	moofBoxes, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoof()})
	if err != nil {
		return info, fmt.Errorf("extract moof: %w", err)
	}
	if len(moofBoxes) == 0 {
		return info, errors.New("missing moof box")
	}
	info.HasMoof = true

	// Extract mdat (at top level)
	if _, err := r.Seek(0, 0); err != nil {
		return info, err
	}
	mdatBoxes, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil {
		return info, fmt.Errorf("extract mdat: %w", err)
	}
	if len(mdatBoxes) == 0 {
		return info, errors.New("missing mdat box")
	}
	info.HasMdat = true

	// Extract tfdt: moof → traf → tfdt
	if _, err := r.Seek(0, 0); err != nil {
		return info, err
	}
	tfdtBoxes, err := mp4.ExtractBoxWithPayload(r, nil, mp4.BoxPath{
		mp4.BoxTypeMoof(),
		mp4.BoxTypeTraf(),
		mp4.BoxTypeTfdt(),
	})
	if err != nil {
		return info, fmt.Errorf("extract tfdt: %w", err)
	}
	if len(tfdtBoxes) == 0 {
		return info, errors.New("missing tfdt box")
	}

	tfdt, ok := tfdtBoxes[0].Payload.(*mp4.Tfdt)
	if !ok {
		return info, errors.New("unexpected tfdt payload type")
	}
	// Tfdt embeds FullBox; Version 1 uses 64-bit BaseMediaDecodeTimeV1.
	if tfdt.GetVersion() == 1 {
		info.BaseMediaDecodeTime = tfdt.BaseMediaDecodeTimeV1
	} else {
		info.BaseMediaDecodeTime = uint64(tfdt.BaseMediaDecodeTimeV0)
	}

	return info, nil
}

// ValidateInit performs a basic sanity check on an init segment (ftyp + moov).
// It verifies that a moov box is present.
func ValidateInit(data []byte) error {
	r := bytes.NewReader(data)
	moovBoxes, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoov()})
	if err != nil {
		return fmt.Errorf("extract moov: %w", err)
	}
	if len(moovBoxes) == 0 {
		return errors.New("missing moov box in init segment")
	}
	return nil
}

// ReadStartPTS opens a media segment file and returns the BaseMediaDecodeTime
// from the tfdt box. It uses seek-based parsing so the file is never fully
// loaded into memory.
func ReadStartPTS(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	boxes, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{
		mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTfdt(),
	})
	if err != nil || len(boxes) == 0 {
		return 0, fmt.Errorf("tfdt not found in %s", path)
	}
	tfdt, ok := boxes[0].Payload.(*mp4.Tfdt)
	if !ok {
		return 0, fmt.Errorf("unexpected tfdt payload type in %s", path)
	}
	if tfdt.GetVersion() == 1 {
		return tfdt.BaseMediaDecodeTimeV1, nil
	}
	return uint64(tfdt.BaseMediaDecodeTimeV0), nil
}

// TimescaleFromInitBytes reads the track timescale from an in-memory init segment
// (moov/trak/mdia/mdhd). Used by the processor to get the actual track timescale
// when the SegmentTemplate timescale differs from the media timescale.
func TimescaleFromInitBytes(data []byte) (uint32, error) {
	r := bytes.NewReader(data)
	boxes, err := mp4.ExtractBoxWithPayload(r, nil, mp4.BoxPath{
		mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd(),
	})
	if err != nil || len(boxes) == 0 {
		return 0, fmt.Errorf("mdhd not found in init segment")
	}
	mdhd, ok := boxes[0].Payload.(*mp4.Mdhd)
	if !ok {
		return 0, fmt.Errorf("unexpected mdhd payload type")
	}
	return mdhd.Timescale, nil
}

// ReadInitTimescale opens an init segment file and returns the timescale from
// the mdhd box (moov/trak/mdia/mdhd).
func ReadInitTimescale(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	boxes, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{
		mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd(),
	})
	if err != nil || len(boxes) == 0 {
		return 0, fmt.Errorf("mdhd not found in %s", path)
	}
	mdhd, ok := boxes[0].Payload.(*mp4.Mdhd)
	if !ok {
		return 0, fmt.Errorf("unexpected mdhd payload type in %s", path)
	}
	return mdhd.Timescale, nil
}

// FindMoovRange scans top-level ISO BMFF box headers in data to locate the moov
// box.  Returns the inclusive byte range [start, end] of the moov box and true
// on success.
//
// data should be the leading bytes of the file; 64 KB is usually enough.
// If the moov box header is found but the declared size extends beyond len(data),
// ok is false — the caller should fetch more bytes and retry.
func FindMoovRange(data []byte) (start, end uint64, ok bool) {
	// Sizes come straight off the wire, so every comparison below stays in
	// uint64 and is written as a subtraction from the remaining length. An
	// earlier version compared int(offset), which turned a wrapped-around
	// offset into a negative int that passed the bounds check and then panicked
	// on the index — on an ingest goroutine, where nothing recovers.
	const headerSize = uint64(8) // 4-byte size + 4-byte type
	const extHeaderSize = uint64(16)

	total := uint64(len(data))
	offset := uint64(0)
	for total-offset >= headerSize {
		boxSize := uint64(data[offset])<<24 |
			uint64(data[offset+1])<<16 |
			uint64(data[offset+2])<<8 |
			uint64(data[offset+3])
		boxType := string(data[offset+4 : offset+8])

		if boxSize == 0 {
			// size==0 means "rest of file" — treat as end of scan
			break
		}
		if boxSize == 1 {
			// 64-bit extended size follows the 4-byte type
			if total-offset < extHeaderSize {
				break
			}
			boxSize = uint64(data[offset+8])<<56 |
				uint64(data[offset+9])<<48 |
				uint64(data[offset+10])<<40 |
				uint64(data[offset+11])<<32 |
				uint64(data[offset+12])<<24 |
				uint64(data[offset+13])<<16 |
				uint64(data[offset+14])<<8 |
				uint64(data[offset+15])
			if boxSize < extHeaderSize {
				break // malformed: smaller than the header it just declared
			}
		}
		if boxSize < headerSize {
			break // malformed
		}

		// total-offset is the bytes left, so this rejects a box running past
		// the probe window without ever forming offset+boxSize.
		if boxSize > total-offset {
			if boxType == "moov" {
				// moov extends beyond our probe window; the caller should
				// fetch more bytes and retry.
				return 0, 0, false
			}
			break
		}

		if boxType == "moov" {
			return offset, offset + boxSize - 1, true
		}
		offset += boxSize
	}
	return 0, 0, false
}

// SidxEntry describes one subsegment from a sidx (Segment Index Box).
type SidxEntry struct {
	Offset   uint64 // absolute byte offset from start of file
	Size     uint64 // size of this subsegment in bytes
	Duration uint32 // subsegment duration in sidx timescale units
}

// ParseSidxBytes parses a sidx box from data (the raw bytes of the sidx region)
// and returns the timescale, earliest presentation time, and per-subsegment
// byte ranges relative to the start of the enclosing file.
//
// idxEnd is the exclusive end byte of the sidx region within the file
// (i.e., indexRange end + 1).  Subsegment offsets are computed as:
//
//	idxEnd + sidx.FirstOffset + sum-of-preceding-subsegment-sizes
//
// Reference-type entries (nested sidx) are silently skipped.
func ParseSidxBytes(data []byte, idxEnd uint64) (timescale uint32, earliest uint64, entries []SidxEntry, err error) {
	r := bytes.NewReader(data)
	boxes, err := mp4.ExtractBoxWithPayload(r, nil, mp4.BoxPath{mp4.BoxTypeSidx()})
	if err != nil || len(boxes) == 0 {
		return 0, 0, nil, fmt.Errorf("sidx box not found")
	}
	sidx, ok := boxes[0].Payload.(*mp4.Sidx)
	if !ok {
		return 0, 0, nil, fmt.Errorf("unexpected sidx payload type")
	}

	timescale = sidx.Timescale
	earliest = sidx.GetEarliestPresentationTime()

	offset := idxEnd + sidx.GetFirstOffset()
	for _, ref := range sidx.References {
		if ref.ReferenceType {
			// Skip nested sidx (index-of-index) references.
			offset += uint64(ref.ReferencedSize)
			continue
		}
		entries = append(entries, SidxEntry{
			Offset:   offset,
			Size:     uint64(ref.ReferencedSize),
			Duration: ref.SubsegmentDuration,
		})
		offset += uint64(ref.ReferencedSize)
	}
	return timescale, earliest, entries, nil
}
