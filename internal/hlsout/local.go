package hlsout

// Local playlist generation: a sibling of the served playlists that addresses
// segments by their path on disk, so a tool opening the channel directory can
// read the asset without the gateway running.
//
// It is deliberately separate from Media. That function emits only a strictly
// contiguous run, because a served playlist that skips a sequence number
// desynchronises players. A local playlist has the opposite requirement: it
// must describe everything on disk, holes included, since the whole reason it
// exists is that the on-disk asset may not be contiguous. HLS can say that
// exactly — every segment carries its own URI, and a numbering break becomes
// an explicit EXT-X-DISCONTINUITY.

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
)

// LocalMasterName is the on-disk name of the HLS local master playlist, a
// sibling of manifest.mpd in the channel directory.
const LocalMasterName = "local.m3u8"

// LocalMediaName returns the name of the local media playlist for the i-th
// representation. Playlists sit beside the master in the channel directory and
// are numbered rather than named after the representation, so that a
// representation ID can never turn into a path.
func LocalMediaName(i int) string { return fmt.Sprintf("local_%d.m3u8", i) }

// LocalMasterTracks uses the same track signalling as served HLS with local URIs.
// Callers supply only tracks with exported media playlists and valid groups.
func LocalMasterTracks(variants []hlsdesc.Variant, o MasterOptions, names map[string]string) []byte {
	return master(variants, o, func(id string) string { return names[id] })
}

// LocalMedia renders one representation's media playlist from its Timeline,
// keeping every published segment, holes included, in the Timeline's period
// order. Every path is relative to channelDir, which is where the playlist
// itself is written, and comes from the index's own record of where each file
// was put, so the playlist cannot name a segment the gateway does not have.
// keys names the separately written, validated key file of each key URI.
func LocalMedia(periods []index.TimelinePeriod, channelDir string, keys map[string]string) ([]byte, error) {
	var entries []playlistEntry
	var all []index.SegmentState
	version := 3
	for pi, period := range periods {
		ordered := append([]index.SegmentState(nil), period.Segments...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].SegNo < ordered[j].SegNo })
		for _, s := range ordered {
			entries = append(entries, playlistEntry{seg: s, periodIdx: pi})
			all = append(all, s)
			if s.InitPath != "" {
				version = 6
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("local playlist: no published segments")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:%d\n#EXT-X-TARGETDURATION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:%d\n", version, ceilMaxDuration(all), entries[0].seg.SegNo)
	lines := segmentLines{
		extinf: "#EXTINF:%.3f,\n",
		uri:    func(e playlistEntry) (string, error) { return relativeTo(e.seg.Path, channelDir) },
		initURI: func(e playlistEntry) (string, error) {
			// Persisted init files are validated clear MP4, even in passthrough mode.
			if e.seg.InitPath == "" {
				return "", nil
			}
			return relativeTo(e.seg.InitPath, channelDir)
		},
		keyLine: func(s index.SegmentState) (string, error) {
			if s.KeyURI == "" {
				return "", nil
			}
			name, ok := keys[s.KeyURI]
			if !ok || len(s.IV) != 16 {
				return "", fmt.Errorf("local playlist: missing key or invalid IV for segment %d", s.SegNo)
			}
			return fmt.Sprintf("#EXT-X-KEY:METHOD=AES-128,URI=%q,IV=0x%s", name, hex.EncodeToString(s.IV)), nil
		},
	}
	if err := lines.write(&b, entries); err != nil {
		return nil, err
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String()), nil
}

func relativeTo(p, channelDir string) (string, error) {
	rel, err := store.RelativeToChannel(p, channelDir)
	if err != nil {
		return "", fmt.Errorf("local playlist: %w", err)
	}
	return rel, nil
}
