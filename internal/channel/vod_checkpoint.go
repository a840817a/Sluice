package channel

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
)

// checkpointVODMedia runs only after ingest has stopped. HLS metadata appends
// are best-effort during live serving, so repair them from the in-memory index
// where possible, then make the segment directory entries durable.
//
// Every failure is returned as a warning rather than an error: the rebuild
// that follows reads whatever is on disk, and it already skips an MPEG-TS
// segment with no sidecar record (index/rebuilder.go), so a missing record
// costs that one segment, not the whole VOD. fMP4 segments carry their own
// timing and need no record.
//
// It does not observe cancellation. It is local disk work inside publication,
// and publication, once started, completes: Stop joins it rather than leaving
// a half-committed VOD behind.
func (m *Manager) checkpointVODMedia(rt *Runtime) []string {
	// Group in-memory segments by their representation directory once. The
	// on-disk walk below also groups by directory; scanning every retained
	// segment for every directory makes a many-representation HLS asset
	// quadratic at precisely the point it is trying to finish.
	knownByDir := make(map[string]map[uint64]index.SegmentState)
	if rt.HLS != nil {
		rt.Index.ForEachRep(func(_ index.RepRef, rep *index.RepresentationState) {
			add := func(seg index.SegmentState) {
				dir := filepath.Dir(seg.Path)
				if knownByDir[dir] == nil {
					knownByDir[dir] = make(map[uint64]index.SegmentState)
				}
				knownByDir[dir][seg.SegNo] = seg
			}
			for _, seg := range rep.Committed() {
				add(seg)
			}
			for _, seg := range rep.Published() {
				add(seg)
			}
		})
	}
	base := filepath.Join(m.vodArtifactDir(rt.ID), "periods")
	if _, err := os.Stat(base); err != nil {
		if os.IsNotExist(err) {
			return nil // no media was ever stored for this channel
		}
		return []string{fmt.Sprintf("checkpoint media: %v", err)}
	}
	var warnings []string
	var dirs []string
	reps := make(map[string]map[uint64]string) // dir -> segment number -> extension
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if rt.HLS == nil {
			return nil
		}
		n, ext, ok := store.ParseSegmentFile(d.Name())
		if ok {
			dir := filepath.Dir(path)
			if reps[dir] == nil {
				reps[dir] = make(map[uint64]string)
			}
			reps[dir][n] = ext
		}
		return nil
	})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("checkpoint media: %v", err))
	}
	for dir, segments := range reps {
		warnings = append(warnings, repairSidecar(dir, segments, knownByDir[dir])...)
	}
	// Segment writers already sync file contents; persist their directory
	// entries bottom-up before making the channel's completed state durable.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := store.SyncDir(dirs[i]); err != nil {
			warnings = append(warnings, fmt.Sprintf("sync %s: %v", dirs[i], err))
		}
	}
	for _, dir := range []string{m.vodArtifactDir(rt.ID), m.gwCfg.Store.DataDir} {
		if err := store.SyncDir(dir); err != nil {
			warnings = append(warnings, fmt.Sprintf("sync %s: %v", dir, err))
		}
	}
	return warnings
}

// repairSidecar rewrites one representation's sidecar so it holds a record for
// every segment on disk that the sidecar or the in-memory index knows. An
// MPEG-TS segment neither knows is reported, because the rebuild skips it; an
// fMP4 one is not, because the rebuild times it from its tfdt box.
func repairSidecar(dir string, segments map[uint64]string, known map[uint64]index.SegmentState) []string {
	path := filepath.Join(dir, store.SidecarFileName)
	records, err := store.ReadSegmentMeta(path)
	if err != nil {
		return []string{fmt.Sprintf("read sidecar %s: %v", path, err)}
	}
	byNo := store.SegmentMetaByNo(records)
	// Index entries know the exact stored path (including format).
	for _, seg := range known {
		byNo[seg.SegNo] = store.SegmentMeta{
			SegNo: seg.SegNo, StartPTS: seg.StartPTS, EndPTS: seg.EndPTS,
			Timescale: seg.Timescale, Discontinuity: seg.Discontinuity,
			KeyURI: seg.KeyURI, IV: hex.EncodeToString(seg.IV),
		}
	}
	numbers := make([]uint64, 0, len(segments))
	for n := range segments {
		numbers = append(numbers, n)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	var warnings []string
	var data []byte
	var missing []uint64
	for _, n := range numbers {
		record, ok := byNo[n]
		if !ok {
			if segments[n] == store.ExtTS {
				missing = append(missing, n)
			}
			continue
		}
		line, err := json.Marshal(record)
		if err != nil {
			return append(warnings, fmt.Sprintf("encode sidecar %s: %v", path, err))
		}
		data = append(append(data, line...), '\n')
	}
	// One warning per directory, however many segments: a status poll carries
	// every warning, and a large stray history would otherwise flood it.
	if len(missing) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d segment(s) in %s have no restart metadata and are left out of the VOD (first: segment %d)", len(missing), dir, missing[0]))
	}
	if err := store.WriteDurable(path, data, 0o644); err != nil {
		warnings = append(warnings, fmt.Sprintf("write sidecar %s: %v", path, err))
	}
	return warnings
}
