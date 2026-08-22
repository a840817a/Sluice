package manifest

import (
	"strings"
	"time"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// HLSVariant describes one representation of an HLS source, as read from its
// master playlist.
type HLSVariant struct {
	RepID     string
	Bandwidth uint64
	Codecs    string
	Width     uint64
	Height    uint64
}

// HLSAudio describes one demuxed audio representation to add to the synthesized
// MPD as its own AdaptationSet.
type HLSAudio struct {
	RepID  string
	Codecs string // e.g. "mp4a.40.2"; derived from the video CODECS when empty
}

// HLSSynthOptions controls MPD synthesis for an HLS source.
type HLSSynthOptions struct {
	// ASID and MediaType must match how the ingest side keyed the index,
	// otherwise the generator cannot find the segments to build a timeline from.
	ASID      string
	MediaType imdp.MediaType
	// MimeType is the AdaptationSet mime type, e.g. "video/mp4".
	MimeType string
	// Audio, when non-empty, adds a separate audio AdaptationSet (demuxed
	// source). AudioASID must match how the ingest side keyed the audio index.
	Audio     []HLSAudio
	AudioASID string
	// VOD renders a static presentation; otherwise dynamic.
	VOD bool
	// AvailabilityStartTime anchors a dynamic presentation. It must be stable
	// across requests — a value that moves makes the live edge jump.
	AvailabilityStartTime time.Time
	// MinimumUpdatePeriod suggests how often clients should reload.
	MinimumUpdatePeriod time.Duration
	// TimeShiftBufferDepth is the DVR depth to advertise.
	TimeShiftBufferDepth time.Duration
}

// SynthesizeHLSMPD builds the upstream manifest an HLS source does not have.
//
// The gateway's MPD generator is written against a parsed upstream MPD. An HLS
// source has none, but everything the generator actually needs is either known
// from the master playlist (representation IDs, bandwidth, codecs, resolution)
// or already in the segment index (timing). Producing a stand-in ParsedMPD lets
// the entire tested generator — segment timeline, live-window projection,
// presentationTimeOffset alignment, VOD duration — be reused unchanged rather
// than duplicated for HLS.
//
// Only fMP4 sources should be published this way. MPEG-TS cannot appear in a
// DASH presentation without remuxing, which this gateway deliberately does not
// do; callers gate on that before calling.
//
// Returns nil when no variant has any published segment, since a presentation
// with nothing in it is not worth serving.
func SynthesizeHLSMPD(variants []HLSVariant, snap index.Snapshot, o HLSSynthOptions) *imdp.ParsedMPD {
	if len(variants) == 0 {
		return nil
	}

	p := &imdp.ParsedMPD{
		Type:                 imdp.PresentationDynamic,
		MinimumUpdatePeriod:  o.MinimumUpdatePeriod,
		TimeShiftBufferDepth: o.TimeShiftBufferDepth,
	}
	if o.VOD {
		p.Type = imdp.PresentationStatic
	} else {
		p.AvailabilityStartTime = o.AvailabilityStartTime
	}

	// When audio is demuxed, a variant's CODECS lists both the video and audio
	// codecs; the video AdaptationSet should advertise only the video part, and
	// the audio AdaptationSet the audio part.
	demuxed := len(o.Audio) > 0

	for _, periodID := range snap.PeriodIDs() {
		var sets []*imdp.ParsedAdaptationSet

		vas := &imdp.ParsedAdaptationSet{
			ID:        o.ASID,
			MediaType: o.MediaType,
			MimeType:  o.MimeType,
		}
		for _, v := range variants {
			// The timescale has to match what the index recorded, because the
			// generator builds the timeline from indexed PTS values using it.
			// It comes from the init segment's mdhd box and can legitimately
			// differ between representations.
			ts := repTimescale(snap, periodID, o.ASID, v.RepID)
			if ts == 0 {
				continue // nothing published for this representation yet
			}
			codecs := v.Codecs
			if demuxed {
				codecs = videoCodecs(v.Codecs)
			}
			vas.Representations = append(vas.Representations, &imdp.ParsedRepresentation{
				ID:        v.RepID,
				Bandwidth: v.Bandwidth,
				Codecs:    codecs,
				Width:     v.Width,
				Height:    v.Height,
				MimeType:  o.MimeType,
				// A SegmentTemplate carrying only the timescale is enough: the
				// generator supplies its own media and init paths, and replaces
				// any fixed duration with a SegmentTimeline built from the index.
				SegTemplate: &imdp.ParsedSegmentTemplate{Timescale: ts},
			})
		}
		if len(vas.Representations) > 0 {
			sets = append(sets, vas)
		}

		if demuxed {
			aas := &imdp.ParsedAdaptationSet{
				ID:        o.AudioASID,
				MediaType: imdp.MediaAudio,
				MimeType:  "audio/mp4",
			}
			for _, a := range o.Audio {
				ts := repTimescale(snap, periodID, o.AudioASID, a.RepID)
				if ts == 0 {
					continue
				}
				codecs := a.Codecs
				if codecs == "" {
					codecs = audioCodecsFrom(variants)
				}
				aas.Representations = append(aas.Representations, &imdp.ParsedRepresentation{
					ID:          a.RepID,
					Codecs:      codecs,
					MimeType:    "audio/mp4",
					SegTemplate: &imdp.ParsedSegmentTemplate{Timescale: ts},
				})
			}
			if len(aas.Representations) > 0 {
				sets = append(sets, aas)
			}
		}

		if len(sets) > 0 {
			p.Periods = append(p.Periods, &imdp.ParsedPeriod{
				ID:             periodID,
				AdaptationSets: sets,
			})
		}
	}

	if len(p.Periods) == 0 {
		return nil
	}
	return p
}

// repTimescale reports the timescale recorded for a representation's published
// segments, or 0 when it has none.
func repTimescale(snap index.Snapshot, periodID, asID, repID string) uint64 {
	for _, s := range snap.Published(periodID, asID, repID) {
		if s.Timescale > 0 {
			return uint64(s.Timescale)
		}
	}
	return 0
}

// videoCodecs returns the comma-separated video codecs from a variant CODECS
// string, dropping any audio codecs (used when audio is demuxed into its own AS).
func videoCodecs(codecs string) string {
	return filterCodecs(codecs, true)
}

// audioCodecsFrom returns the audio codec found in the variants' CODECS, for an
// audio rendition whose EXT-X-MEDIA carried no codec of its own.
func audioCodecsFrom(variants []HLSVariant) string {
	for _, v := range variants {
		if a := filterCodecs(v.Codecs, false); a != "" {
			return a
		}
	}
	return "mp4a.40.2" // sensible default: AAC-LC
}

// filterCodecs keeps either the video or the audio codecs from a CODECS list.
func filterCodecs(codecs string, wantVideo bool) string {
	var kept []string
	for _, c := range strings.Split(codecs, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if isVideoCodec(c) == wantVideo {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, ",")
}

// isVideoCodec classifies a RFC 6381 codec string as video (vs audio).
func isVideoCodec(c string) bool {
	for _, p := range []string{"avc1", "avc3", "hev1", "hvc1", "vp08", "vp09", "av01", "dvh1", "dvhe"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}
