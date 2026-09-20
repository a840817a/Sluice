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
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/a840817a/sluice/internal/index"
)

// LocalMasterName is the on-disk name of the HLS local master playlist, a
// sibling of manifest.mpd in the channel directory.
const LocalMasterName = "local.m3u8"

// LocalMediaName returns the name of the local media playlist for the i-th
// representation. Playlists sit beside the master in the channel directory and
// are numbered rather than named after the representation, so that a
// representation ID can never turn into a path.
func LocalMediaName(i int) string { return fmt.Sprintf("local_%d.m3u8", i) }

// LocalRendition is one representation's contribution to the local master.
type LocalRendition struct {
	Ref       index.RepRef
	Segments  []index.SegmentState
	Bandwidth uint64
	Codecs    string
	Width     int
	Height    int
}

// LocalMaster renders the master playlist. Renditions are listed in the order
// given; each entry points at LocalMediaName(i).
func LocalMaster(rends []LocalRendition) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")
	for i, r := range rends {
		attrs := []string{fmt.Sprintf("BANDWIDTH=%d", maxU64(r.Bandwidth, 1))}
		if r.Width > 0 && r.Height > 0 {
			attrs = append(attrs, fmt.Sprintf("RESOLUTION=%dx%d", r.Width, r.Height))
		}
		if r.Codecs != "" {
			attrs = append(attrs, fmt.Sprintf("CODECS=%q", r.Codecs))
		}
		b.WriteString("#EXT-X-STREAM-INF:" + strings.Join(attrs, ",") + "\n")
		b.WriteString(LocalMediaName(i) + "\n")
	}
	return []byte(b.String())
}

// LocalMedia renders one representation's media playlist. Every path is
// relative to channelDir, which is where the playlist itself is written.
//
// Paths come from the index's own record of where each file was put, so the
// playlist cannot name a segment the gateway does not have.
func LocalMedia(segs []index.SegmentState, channelDir string) ([]byte, error) {
	if len(segs) == 0 {
		return nil, fmt.Errorf("local playlist: no published segments")
	}
	ordered := append([]index.SegmentState(nil), segs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SegNo < ordered[j].SegNo })

	version := 3
	if ordered[0].InitPath != "" {
		version = 6 // EXT-X-MAP requires 6
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", ceilMaxDuration(ordered))
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", ordered[0].SegNo)

	if init := ordered[0].InitPath; init != "" {
		rel, err := relativeTo(init, channelDir)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", rel)
	}

	prev := ordered[0].SegNo
	for i, s := range ordered {
		// A break in numbering is a break in the timeline. Saying so is what
		// keeps a reader from treating the jump as continuous media.
		if i > 0 && (s.SegNo != prev+1 || s.Discontinuity) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		prev = s.SegNo

		rel, err := relativeTo(s.Path, channelDir)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", segmentDuration(s), rel)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String()), nil
}

func relativeTo(p, channelDir string) (string, error) {
	rel, err := filepath.Rel(channelDir, p)
	if err != nil {
		return "", fmt.Errorf("local playlist: %q is not under %q: %w", p, channelDir, err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("local playlist: %q escapes the channel directory", p)
	}
	return filepath.ToSlash(rel), nil
}

func maxU64(v, min uint64) uint64 {
	if v < min {
		return min
	}
	return v
}
