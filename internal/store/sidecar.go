package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// SidecarFileName is the per-representation segment metadata file.
const SidecarFileName = "segments.jsonl"

// SegmentMeta is the metadata for one stored segment that cannot be recovered
// by reading the segment file itself.
//
// fMP4 carries its own timing in a tfdt box, so a DASH channel can always be
// rebuilt from the files alone. MPEG-TS carries none: its presentation time
// comes from the playlist's EXTINF durations, and its encryption state from
// EXT-X-KEY. Without this record a restarted HLS channel cannot reconstruct its
// DVR window at all.
//
// Field names are short because one record is written per segment and a
// long-running channel accumulates a lot of them.
type SegmentMeta struct {
	SegNo         uint64 `json:"n"`
	StartPTS      int64  `json:"s"`
	EndPTS        int64  `json:"e"`
	Timescale     uint32 `json:"t"`
	Discontinuity bool   `json:"dc,omitempty"`
	// KeyURI and IV are set only for segments stored as ciphertext, so the
	// outbound playlist can re-advertise EXT-X-KEY after a restart. IV is hex.
	KeyURI string `json:"k,omitempty"`
	IV     string `json:"iv,omitempty"`
}

// SidecarPath returns the metadata file for one representation. It lives beside
// that representation's segments.
//
//	{dataDir}/{channelID}/periods/{periodID}/{mediaType}_{asID}/{repID}/segments.jsonl
func SidecarPath(dataDir, channelID, periodID, mediaType, asID, repID string) string {
	return filepath.Join(
		dataDir,
		channelID,
		"periods",
		periodID,
		asDirName(mediaType, asID),
		repID,
		SidecarFileName,
	)
}

// AppendSegmentMeta appends one record.
//
// Appending rather than rewriting keeps the per-segment cost constant and means
// a crash can only ever damage the final line, which ReadSegmentMeta discards.
func AppendSegmentMeta(path string, m SegmentMeta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// ReadSegmentMeta reads every intact record, in file order.
//
// A missing file is not an error: a representation that has committed nothing
// yet simply has no sidecar. Unparseable lines are skipped rather than failing
// the read, so one torn line from a crash cannot cost the whole DVR window.
func ReadSegmentMeta(path string) ([]SegmentMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []SegmentMeta
	sc := bufio.NewScanner(f)
	// Records are small, but give the scanner room so an unusually long line
	// cannot abort the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m SegmentMeta
		if err := json.Unmarshal(line, &m); err != nil {
			continue // torn or corrupt line
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		// Return what was recovered; a read error partway through still leaves
		// the earlier records usable.
		return out, nil
	}
	return out, nil
}

// SegmentMetaByNo indexes records by segment number, keeping the last record
// written for each. A segment refetched after expiry appends a fresh record,
// and the newest one is the truth.
func SegmentMetaByNo(records []SegmentMeta) map[uint64]SegmentMeta {
	out := make(map[uint64]SegmentMeta, len(records))
	for _, m := range records {
		out[m.SegNo] = m
	}
	return out
}

// CompactSegmentMeta rewrites the sidecar keeping one record per segment for
// which keep returns true, in segment order.
//
// This is what bounds the file: records for segments that have been expired off
// disk are dropped, and duplicates collapse. The rewrite goes to a temporary
// file and is renamed into place, so a crash during compaction leaves the
// previous sidecar intact.
func CompactSegmentMeta(path string, keep func(segNo uint64) bool) error {
	records, err := ReadSegmentMeta(path)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}

	byNo := SegmentMetaByNo(records)
	kept := make([]SegmentMeta, 0, len(byNo))
	for segNo, m := range byNo {
		if keep(segNo) {
			kept = append(kept, m)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].SegNo < kept[j].SegNo })

	var buf []byte
	for _, m := range kept {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	return Write(path, buf)
}
