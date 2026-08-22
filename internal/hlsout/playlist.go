// Package hlsout renders outbound HLS playlists from the channel index.
//
// It is the HLS counterpart of internal/manifest: given the segments this
// gateway currently holds, it produces the master and media playlists that
// point clients back at the gateway's own segment endpoints. It never touches
// segment bytes — HLS sources are served through untouched.
package hlsout

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
)

// The track descriptions this package renders are defined in internal/hlsdesc,
// shared with the ingest side that discovers them.

// MasterOptions controls master playlist rendering.
type MasterOptions struct {
	GatewayBaseURL string
	ChannelID      string
	// IndependentSegments mirrors the upstream EXT-X-INDEPENDENT-SEGMENTS tag.
	IndependentSegments bool
	// Renditions are demuxed audio tracks, emitted as EXT-X-MEDIA and bound to
	// variants by their AudioGroup.
	Renditions []hlsdesc.Rendition
	// Subtitles are demuxed WebVTT tracks, emitted as EXT-X-MEDIA:TYPE=SUBTITLES.
	Subtitles []hlsdesc.Subtitle
}

// MediaOptions controls media playlist rendering.
type MediaOptions struct {
	GatewayBaseURL string
	ChannelID      string
	RepID          string

	// VOD renders a terminated, full-history playlist.
	VOD bool
	// WindowSegments caps a live playlist to the newest N segments. Zero means
	// no cap.
	WindowSegments int
	// TargetDuration is the value to advertise. Callers must source this from a
	// StickyTargetDuration so it never decreases between reloads.
	TargetDuration int
	// InitURI, when non-empty, emits EXT-X-MAP and marks the playlist as fMP4.
	InitURI string
	// KeyProxyBase is the base URL of this gateway's key proxy, e.g.
	// "https://gw/v1/channels/ch1/key". Segments stored as ciphertext get an
	// EXT-X-KEY pointing under it. When empty no key is advertised at all —
	// publishing the upstream key URL instead would hand clients a credential
	// the gateway was meant to broker.
	KeyProxyBase string
	// DiscontinuitySequence is the number of discontinuities that have already
	// slid out of the window. It cannot be derived from the window's contents,
	// so the caller maintains it as a running per-representation counter.
	DiscontinuitySequence uint64
}

// Master renders the multivariant playlist. Every variant URI points at this
// gateway's media playlist endpoint.
func Master(variants []hlsdesc.Variant, o MasterOptions) []byte {
	base := strings.TrimRight(o.GatewayBaseURL, "/")

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	if o.IndependentSegments {
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}

	// Demuxed audio tracks, each pointing at its gateway media playlist.
	for _, r := range o.Renditions {
		attrs := []string{
			"TYPE=AUDIO",
			fmt.Sprintf("GROUP-ID=%q", r.GroupID),
			fmt.Sprintf("NAME=%q", r.Name),
		}
		if r.Language != "" {
			attrs = append(attrs, fmt.Sprintf("LANGUAGE=%q", r.Language))
		}
		if r.Channels != "" {
			attrs = append(attrs, fmt.Sprintf("CHANNELS=%q", r.Channels))
		}
		if r.Default {
			attrs = append(attrs, "DEFAULT=YES", "AUTOSELECT=YES")
		}
		attrs = append(attrs, fmt.Sprintf("URI=%q",
			gwurl.MediaPlaylistURL(base, o.ChannelID, r.RepID)))
		b.WriteString("#EXT-X-MEDIA:" + strings.Join(attrs, ",") + "\n")
	}

	// Demuxed subtitle tracks.
	for _, s := range o.Subtitles {
		attrs := []string{
			"TYPE=SUBTITLES",
			fmt.Sprintf("GROUP-ID=%q", s.GroupID),
			fmt.Sprintf("NAME=%q", s.Name),
		}
		if s.Language != "" {
			attrs = append(attrs, fmt.Sprintf("LANGUAGE=%q", s.Language))
		}
		if s.Default {
			attrs = append(attrs, "DEFAULT=YES", "AUTOSELECT=YES")
		}
		if s.Forced {
			attrs = append(attrs, "FORCED=YES")
		}
		attrs = append(attrs, fmt.Sprintf("URI=%q",
			gwurl.MediaPlaylistURL(base, o.ChannelID, s.RepID)))
		b.WriteString("#EXT-X-MEDIA:" + strings.Join(attrs, ",") + "\n")
	}

	for _, v := range variants {
		attrs := []string{fmt.Sprintf("BANDWIDTH=%d", v.Bandwidth)}
		if v.AverageBandwidth > 0 {
			attrs = append(attrs, fmt.Sprintf("AVERAGE-BANDWIDTH=%d", v.AverageBandwidth))
		}
		if v.Width > 0 && v.Height > 0 {
			attrs = append(attrs, fmt.Sprintf("RESOLUTION=%dx%d", v.Width, v.Height))
		}
		if v.FrameRate > 0 {
			attrs = append(attrs, fmt.Sprintf("FRAME-RATE=%.3f", v.FrameRate))
		}
		if v.Codecs != "" {
			attrs = append(attrs, fmt.Sprintf("CODECS=%q", v.Codecs))
		}
		if v.AudioGroup != "" {
			attrs = append(attrs, fmt.Sprintf("AUDIO=%q", v.AudioGroup))
		}
		if v.SubtitleGroup != "" {
			attrs = append(attrs, fmt.Sprintf("SUBTITLES=%q", v.SubtitleGroup))
		}
		b.WriteString("#EXT-X-STREAM-INF:" + strings.Join(attrs, ",") + "\n")
		b.WriteString(gwurl.MediaPlaylistURL(base, o.ChannelID, v.RepID) + "\n")
	}

	return []byte(b.String())
}

// Media renders a media playlist for one representation from the segments the
// gateway currently holds.
//
// Only a strictly contiguous run of segments is ever emitted: a playlist that
// skips a sequence number desynchronises players. Which run is chosen depends
// on the mode — see contiguousRun.
func Media(segs []index.SegmentState, o MediaOptions) []byte {
	base := strings.TrimRight(o.GatewayBaseURL, "/")

	run := contiguousRun(segs, o.VOD)
	if o.WindowSegments > 0 && len(run) > o.WindowSegments {
		run = run[len(run)-o.WindowSegments:]
	}

	// EXT-X-VERSION 6 is required for EXT-X-MAP in a media playlist; plain
	// MPEG-TS playlists remain at 3 for maximum client compatibility.
	version := 3
	if o.InitURI != "" {
		version = 6
	}

	targetDur := o.TargetDuration
	if targetDur <= 0 {
		targetDur = ceilMaxDuration(run)
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)

	var mediaSeq uint64
	if len(run) > 0 {
		mediaSeq = run[0].SegNo
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)

	if o.DiscontinuitySequence > 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", o.DiscontinuitySequence)
	}
	if o.VOD {
		b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	}
	if o.InitURI != "" {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", o.InitURI)
	}

	// keyLine tracks the EXT-X-KEY currently in force so it is re-emitted only
	// when it actually changes.
	var keyLine string
	for i, s := range run {
		// The leading discontinuity of the emitted run is meaningless: it
		// describes a break from a segment the client never saw, and
		// EXT-X-DISCONTINUITY-SEQUENCE already accounts for it.
		if s.Discontinuity && i > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if line := keyLineFor(s, o.KeyProxyBase); line != keyLine {
			keyLine = line
			if line != "" {
				b.WriteString(line + "\n")
			} else if i > 0 {
				// Encryption stopped part-way through: players must be told, or
				// they keep applying the previous key.
				b.WriteString("#EXT-X-KEY:METHOD=NONE\n")
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n", segmentDuration(s))
		b.WriteString(gwurl.SegmentURL(base, o.ChannelID, o.RepID, s.SegNo, segmentExt(s)) + "\n")
	}

	if o.VOD {
		b.WriteString("#EXT-X-ENDLIST\n")
	}

	return []byte(b.String())
}

// keyLineFor renders the EXT-X-KEY tag a segment needs, or "" when the segment
// is cleartext (or no key proxy is configured to publish).
//
// The IV is always emitted explicitly rather than relying on the client
// deriving it from the media sequence number: the gateway renumbers segments
// relative to the upstream, so an implicit IV would be computed from the wrong
// value and decryption would silently produce garbage.
func keyLineFor(s index.SegmentState, keyProxyBase string) string {
	if s.KeyURI == "" || keyProxyBase == "" {
		return ""
	}
	uri := strings.TrimRight(keyProxyBase, "/") + "/" + hlskey.KeyID(s.KeyURI)
	return fmt.Sprintf("#EXT-X-KEY:METHOD=AES-128,URI=%q,IV=0x%s", uri, hex.EncodeToString(s.IV))
}

// contiguousRun returns the longest run of consecutive segment numbers.
//
// For VOD the run is anchored at the oldest segment: the history is meant to be
// complete, so a hole means everything after it is untrustworthy and is cut.
// For live the run is anchored at the newest segment, so that a permanently
// lost segment does not stall the playlist at the hole forever — the gateway
// keeps following the live edge.
func contiguousRun(segs []index.SegmentState, vod bool) []index.SegmentState {
	if len(segs) == 0 {
		return nil
	}

	sorted := make([]index.SegmentState, len(segs))
	copy(sorted, segs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SegNo < sorted[j].SegNo })

	// A repeated segment number is not a break in coverage, but it fails the
	// "next == prev+1" test below and would truncate the playlist to a couple of
	// segments — a silent stall for every player. The index collapses duplicates
	// before they reach here; this keeps a regression upstream from turning into
	// an outage.
	uniq := sorted[:1]
	for _, s := range sorted[1:] {
		if s.SegNo == uniq[len(uniq)-1].SegNo {
			uniq[len(uniq)-1] = s // keep the later state
			continue
		}
		uniq = append(uniq, s)
	}
	sorted = uniq

	if vod {
		end := 1
		for end < len(sorted) && sorted[end].SegNo == sorted[end-1].SegNo+1 {
			end++
		}
		return sorted[:end]
	}

	start := len(sorted) - 1
	for start > 0 && sorted[start].SegNo == sorted[start-1].SegNo+1 {
		start--
	}
	return sorted[start:]
}

// segmentDuration returns the segment's duration in seconds. For TS sources
// this round-trips the upstream EXTINF, which was stored as a PTS span at
// timescale 90000.
func segmentDuration(s index.SegmentState) float64 {
	if s.Timescale == 0 {
		return 0
	}
	d := float64(s.EndPTS-s.StartPTS) / float64(s.Timescale)
	if d < 0 {
		return 0
	}
	return d
}

// segmentExt returns the stored extension so the outbound URI matches the file
// actually on disk.
func segmentExt(s index.SegmentState) string {
	switch {
	case strings.HasSuffix(s.Path, store.ExtTS):
		return store.ExtTS
	case strings.HasSuffix(s.Path, store.ExtVTT):
		return store.ExtVTT
	default:
		return store.ExtFMP4
	}
}

// ceilMaxDuration returns the smallest integer >= the longest segment duration,
// which is the floor RFC 8216 places on EXT-X-TARGETDURATION.
func ceilMaxDuration(segs []index.SegmentState) int {
	max := 0.0
	for _, s := range segs {
		if d := segmentDuration(s); d > max {
			max = d
		}
	}
	return int(math.Ceil(max))
}

// StickyTargetDuration tracks EXT-X-TARGETDURATION per representation and only
// ever lets it grow.
//
// RFC 8216 §4.3.3.1 requires the advertised target duration to bound every
// EXTINF in the playlist, and clients cache it across reloads. Recomputing it
// per request from the current window can shrink it below a previously
// advertised value, which players surface as intermittent, hard-to-diagnose
// stalls. The zero value is ready to use.
type StickyTargetDuration struct {
	mu  sync.Mutex
	max map[string]int
}

// Get returns the target duration to advertise for repID given the segments
// about to be rendered, never returning less than any earlier call for the same
// representation.
func (s *StickyTargetDuration) Get(repID string, segs []index.SegmentState) int {
	d := ceilMaxDuration(segs)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.max == nil {
		s.max = make(map[string]int)
	}
	if d > s.max[repID] {
		s.max[repID] = d
	}
	return s.max[repID]
}
