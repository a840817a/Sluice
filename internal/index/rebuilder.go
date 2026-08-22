package index

import (
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/a840817a/sluice/internal/store"
	"github.com/a840817a/sluice/internal/validate"
)

// rebuildRingCapacity is the fallback ring size for RebuildFromDisk, which has
// no channel config to consult. It deliberately does NOT share a declaration
// with pipeline.SegmentCapacity's cap: index must not import config, and callers
// that know the channel's retention policy use RebuildFromDiskWithCapacity and
// pass the value in. The two happening to be equal is a default, not a coupling.
const rebuildRingCapacity = 120

// RebuildFromDisk scans the data directory and populates ci with COMMITTED
// SegmentState entries for every .m4s file found, then runs the A/V barrier
// so that only properly time-aligned segments are published.
//
// Expected layout (matching store/layout.go):
//
//	{dataDir}/{channelID}/periods/{periodID}/{mediaType}_{asID}/{repID}/{segNo}.m4s
//	{dataDir}/{channelID}/periods/{periodID}/{mediaType}_{asID}/{repID}/init.mp4
//
// The {mediaType}_{asID} directory name encodes both the media type and the
// AdaptationSet positional ID with a single underscore separator, e.g. "video_0",
// "audio_1", "audio_2".  strings.LastIndex is used to split on the separator so
// that media type names that cannot contain underscores ("video", "audio", "text")
// are always handled correctly.
//
// Each .m4s file is opened (seek-based, not fully loaded) to read
// BaseMediaDecodeTime from the tfdt box.  EndPTS is derived from the next
// segment's StartPTS; for the last segment the previous delta is reused.
//
// Segments are committed via rep.Commit() and then the A/V barrier is run
// per period so that only time-aligned segments become Published.
// RebuildFromDiskForVOD is identical to RebuildFromDisk but creates each
// RepresentationState with an unbounded ring buffer (capacity=0). This allows
// the full segment history of a live stream to be loaded into memory after a
// live→VOD transition, regardless of the stream length.
func RebuildFromDiskForVOD(ci *ChannelIndex, dataDir, channelID string) error {
	return rebuildFromDisk(ci, dataDir, channelID, 0)
}

// RebuildFromDisk scans the data directory and populates ci with COMMITTED
// SegmentState entries for every .m4s file found, then runs the A/V barrier
// so that only properly time-aligned segments are published.
func RebuildFromDisk(ci *ChannelIndex, dataDir, channelID string) error {
	return rebuildFromDisk(ci, dataDir, channelID, rebuildRingCapacity)
}

// RebuildFromDiskWithCapacity is like RebuildFromDisk but lets the caller
// choose the ring buffer capacity per representation. Pass 0 for unbounded
// (e.g. when the channel keeps all segments or is in VOD mode).
func RebuildFromDiskWithCapacity(ci *ChannelIndex, dataDir, channelID string, capacity int) error {
	return rebuildFromDisk(ci, dataDir, channelID, capacity)
}

func rebuildFromDisk(ci *ChannelIndex, dataDir, channelID string, capacity int) error {
	base := filepath.Join(dataDir, channelID, "periods")
	periodEntries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, periodEntry := range periodEntries {
		if !periodEntry.IsDir() {
			continue
		}
		periodID := periodEntry.Name()
		ps := ci.Period(periodID)

		// Scan {mediaType}_{asID} directories.
		asEntries, err := os.ReadDir(filepath.Join(base, periodID))
		if err != nil {
			continue
		}
		for _, asEntry := range asEntries {
			if !asEntry.IsDir() {
				continue
			}
			// Split "video_0" → mediaType="video", asID="0"
			asDirName := asEntry.Name()
			sep := strings.LastIndex(asDirName, "_")
			if sep < 0 {
				continue // unexpected directory name, skip
			}
			mediaType := asDirName[:sep]
			asID := asDirName[sep+1:]

			as := ps.AS(asID)
			as.SetMediaType(mediaType)

			asPath := filepath.Join(base, periodID, asDirName)
			repEntries, err := os.ReadDir(asPath)
			if err != nil {
				continue
			}
			for _, repEntry := range repEntries {
				if !repEntry.IsDir() {
					continue
				}
				repID := repEntry.Name()
				rep := as.Rep(repID, capacity)
				repPath := filepath.Join(asPath, repID)
				initPath := filepath.Join(repPath, store.InitFileName)

				// Read timescale from the init segment (best-effort).
				timescale, _ := validate.ReadInitTimescale(initPath)

				// MPEG-TS segments carry no tfdt box, so their timing is not
				// recoverable from the files. It comes from the sidecar the
				// processor writes on commit.
				sidecar, err := store.ReadSegmentMeta(
					filepath.Join(repPath, store.SidecarFileName))
				if err != nil {
					slog.Warn("segment metadata read failed", "rep", repID, "err", err)
				}
				metaByNo := store.SegmentMetaByNo(sidecar)

				// First pass: collect (segNo, startPTS) for each fMP4 segment,
				// whose EndPTS has to be derived from its neighbours.
				type segEntry struct {
					segNo    uint64
					startPTS int64
					path     string
				}
				var segs []segEntry
				// TS segments are fully described by their sidecar record and
				// need no neighbour arithmetic.
				var tsStates []SegmentState

				segFiles, err := os.ReadDir(repPath)
				if err != nil {
					continue
				}
				for _, sf := range segFiles {
					name := sf.Name()
					segNo, ext, ok := store.ParseSegmentFile(name)
					if !ok {
						continue
					}
					segPath := filepath.Join(repPath, name)

					if ext == store.ExtTS {
						m, ok := metaByNo[segNo]
						if !ok {
							// No record: the segment cannot be placed on the
							// timeline, so serving it would corrupt the playlist.
							continue
						}
						st := SegmentState{
							SegNo:         segNo,
							StartPTS:      m.StartPTS,
							EndPTS:        m.EndPTS,
							Timescale:     m.Timescale,
							Path:          segPath,
							Discontinuity: m.Discontinuity,
							KeyURI:        m.KeyURI,
						}
						// Segments stored as ciphertext keep their key material so
						// the playlist can re-advertise EXT-X-KEY after a restart.
						if m.IV != "" {
							if iv, err := hex.DecodeString(m.IV); err == nil {
								st.IV = iv
							}
						}
						tsStates = append(tsStates, st)
						continue
					}

					pts, err := validate.ReadStartPTS(segPath)
					if err != nil {
						continue // skip unreadable segments
					}
					segs = append(segs, segEntry{
						segNo:    segNo,
						startPTS: int64(pts),
						path:     segPath,
					})
				}

				// Commit in segment order. Directory listings are lexical
				// ("10.ts" before "2.ts"), and a bounded ring buffer keeps the
				// last N pushed — so committing out of order would retain an
				// arbitrary subset instead of the newest segments.
				sort.Slice(tsStates, func(i, j int) bool {
					return tsStates[i].SegNo < tsStates[j].SegNo
				})
				for _, st := range tsStates {
					rep.Commit(st)
				}

				// Sort ascending by segment number so EndPTS can be derived
				// from the next segment's StartPTS.
				sort.Slice(segs, func(i, j int) bool {
					return segs[i].segNo < segs[j].segNo
				})

				// Second pass: commit each segment so the A/V barrier can run.
				for i, se := range segs {
					endPTS := se.startPTS
					switch {
					case i+1 < len(segs):
						// End time = start of next segment.
						endPTS = segs[i+1].startPTS
					case i > 0:
						// Last segment: estimate from previous segment's duration.
						endPTS = se.startPTS + (segs[i].startPTS - segs[i-1].startPTS)
					}
					st := SegmentState{
						SegNo:     se.segNo,
						StartPTS:  se.startPTS,
						EndPTS:    endPTS,
						Timescale: timescale,
						Path:      se.path,
						InitPath:  initPath,
					}
					// fMP4 timing comes from the tfdt box, but its discontinuity
					// flag and (for encrypted passthrough) key material live only
					// in the sidecar. DASH channels have no sidecar, so this is a
					// no-op for them.
					if m, ok := metaByNo[se.segNo]; ok {
						st.Discontinuity = m.Discontinuity
						st.KeyURI = m.KeyURI
						if m.IV != "" {
							if iv, err := hex.DecodeString(m.IV); err == nil {
								st.IV = iv
							}
						}
					}
					rep.Commit(st)
				}
			}
		}

		// After all AdaptationSets for this period are committed, run the
		// A/V barrier so only time-aligned segments are published.
		NewBarrier(ps, 0.5).CheckAndPublish()
	}
	return nil
}
