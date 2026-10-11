package manifest

// Local manifest generation: a sibling of the served manifest that addresses
// segments by their path on disk rather than by the gateway's URL shape, so a
// tool pointed at the channel directory can read the asset directly.
//
// It is deliberately not built from the served generator. go-mpd, which
// marshals that manifest, has no SegmentList type at all — buildRepFromSegmentList
// exists precisely because a SegmentList *source* has to become a SegmentTemplate
// *output*. A template cannot express a hole in segment numbering, and holes are
// the normal case here: a live channel sheds segments, and a static ingest can
// leave stragglers far past its contiguous run. So this writer emits its own
// small document with an explicit per-segment list, and the served path stays
// untouched.

import (
	"encoding/xml"
	"fmt"
	"math"
	"strings"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/store"
)

// LocalManifestName is the on-disk name of the DASH local manifest, a sibling
// of manifest.mpd in the channel directory.
const LocalManifestName = "local.mpd"

type localMPD struct {
	XMLName                   xml.Name      `xml:"MPD"`
	Xmlns                     string        `xml:"xmlns,attr"`
	XmlnsCenc                 string        `xml:"xmlns:cenc,attr,omitempty"`
	XmlnsMspr                 string        `xml:"xmlns:mspr,attr,omitempty"`
	Type                      string        `xml:"type,attr"`
	Profiles                  string        `xml:"profiles,attr"`
	MinBufferTime             string        `xml:"minBufferTime,attr"`
	MediaPresentationDuration string        `xml:"mediaPresentationDuration,attr"`
	Periods                   []localPeriod `xml:"Period"`
}

type localPeriod struct {
	Start          string    `xml:"start,attr"`
	Duration       string    `xml:"duration,attr"`
	ID             string    `xml:"id,attr,omitempty"`
	AdaptationSets []localAS `xml:"AdaptationSet"`
}

// RawCPs carries the ContentProtection elements verbatim. encoding/xml drops
// namespace-prefixed attributes and children (cenc:default_KID, cenc:pssh,
// mspr:pro), which is the same reason the served manifest re-injects them, so
// the parser's preserved source text is written through untouched. It relies
// on the root declaring cenc and mspr, as imdp.ParsedAdaptationSet.RawCPs
// requires.
type localAS struct {
	MimeType string     `xml:"mimeType,attr,omitempty"`
	Lang     string     `xml:"lang,attr,omitempty"`
	RawCPs   string     `xml:",innerxml"`
	Reps     []localRep `xml:"Representation"`
}

type localRep struct {
	MimeType    string           `xml:"mimeType,attr,omitempty"`
	ID          string           `xml:"id,attr"`
	Width       uint64           `xml:"width,attr,omitempty"`
	Height      uint64           `xml:"height,attr,omitempty"`
	Bandwidth   uint64           `xml:"bandwidth,attr,omitempty"`
	Codecs      string           `xml:"codecs,attr,omitempty"`
	SegmentList localSegmentList `xml:"SegmentList"`
}

type localSegmentList struct {
	PresentationTimeOffset uint64         `xml:"presentationTimeOffset,attr,omitempty"`
	Timescale              uint32         `xml:"timescale,attr,omitempty"`
	Timeline               *localTimeline `xml:"SegmentTimeline,omitempty"`
	Init                   *localInit     `xml:"Initialization,omitempty"`
	URLs                   []localSegURL  `xml:"SegmentURL"`
}

type localTimeline struct {
	S []localS `xml:"S"`
}

type localS struct {
	T uint64 `xml:"t,attr"`
	D uint64 `xml:"d,attr"`
	R int64  `xml:"r,attr,omitempty"`
}

type localInit struct {
	SourceURL string `xml:"sourceURL,attr"`
}

type localSegURL struct {
	Media string `xml:"media,attr"`
}

// GenerateLocalMPD renders a manifest whose segment references are paths
// relative to channelDir — the directory the manifest itself is written into —
// so that resolving them against the file's own location finds the segments on
// disk.
//
// Every path comes from the index's own record of where it put the file, not
// from re-deriving the store's layout, so the manifest cannot name a file the
// gateway does not have.
func GenerateLocalMPD(orig *imdp.ParsedMPD, snap index.Snapshot, channelDir string) ([]byte, error) {
	if orig == nil {
		return nil, fmt.Errorf("local manifest: no parsed MPD")
	}

	doc := localMPD{
		Xmlns:         "urn:mpeg:dash:schema:mpd:2011",
		XmlnsCenc:     imdp.NamespaceCENC,
		XmlnsMspr:     imdp.NamespaceMSPR,
		Type:          "static",
		Profiles:      "urn:mpeg:dash:profile:isoff-main:2011",
		MinBufferTime: "PT2S",
	}
	if orig.Raw != nil && orig.Raw.Profiles != "" {
		doc.Profiles = orig.Raw.Profiles
	}

	var maxEndSec float64
	for _, origPeriod := range orig.Periods {
		// Rebase all tracks together, never each representation separately: doing
		// so would erase intentional A/V or subtitle offsets. Source PTO may
		// use a different clock from the persisted media timescale.
		begin, end := math.Inf(1), math.Inf(-1)
		for _, as := range origPeriod.AdaptationSets {
			for _, rep := range as.Representations {
				offset := localSourceOffset(rep, as)
				for _, seg := range snap.Published(origPeriod.ID, as.ID, rep.ID) {
					if seg.Timescale == 0 || seg.StartPTS < 0 || seg.EndPTS <= seg.StartPTS {
						return nil, fmt.Errorf("local manifest: invalid segment timing")
					}
					begin = math.Min(begin, seg.StartSec()-offset)
					end = math.Max(end, seg.EndSec()-offset)
				}
			}
		}
		if math.IsInf(begin, 1) {
			continue
		}
		span := end - begin
		lp := localPeriod{ID: origPeriod.ID, Start: fmt.Sprintf("PT%.6fS", maxEndSec), Duration: fmt.Sprintf("PT%.6fS", span)}
		for _, origAS := range origPeriod.AdaptationSets {
			las := localAS{
				MimeType: origAS.MimeType,
				Lang:     origAS.Lang,
				RawCPs:   strings.Join(origAS.RawCPs, "\n"),
			}
			for _, origRep := range origAS.Representations {
				segs := snap.Published(origPeriod.ID, origAS.ID, origRep.ID)
				if len(segs) == 0 {
					// A representation with nothing on disk is left out
					// entirely rather than named with an empty list: a reader
					// should not be offered a rendition it cannot play.
					continue
				}
				sl, _, err := buildLocalSegmentList(segs, channelDir)
				if err != nil {
					return nil, err
				}
				offsetTicks := (localSourceOffset(origRep, origAS) + begin) * float64(sl.Timescale)
				if offsetTicks < -0.5 {
					return nil, fmt.Errorf("local manifest: negative presentation offset for %s", origRep.ID)
				}
				sl.PresentationTimeOffset = uint64(math.Max(0, math.Round(offsetTicks)))
				las.Reps = append(las.Reps, localRep{
					ID:          origRep.ID,
					MimeType:    origRep.MimeType,
					Width:       origRep.Width,
					Height:      origRep.Height,
					Bandwidth:   origRep.Bandwidth,
					Codecs:      origRep.Codecs,
					SegmentList: sl,
				})
			}
			if len(las.Reps) > 0 {
				lp.AdaptationSets = append(lp.AdaptationSets, las)
			}
		}
		if len(lp.AdaptationSets) > 0 {
			doc.Periods = append(doc.Periods, lp)
			maxEndSec += span
		}
	}

	if len(doc.Periods) == 0 {
		return nil, fmt.Errorf("local manifest: no published segments")
	}

	// The duration describes what is actually on disk, which for a channel that
	// lost segments is shorter than what the origin advertised. Announcing
	// the origin's figure would describe an asset this file cannot deliver.
	doc.MediaPresentationDuration = fmt.Sprintf("PT%.3fS", maxEndSec)

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("local manifest: marshal: %w", err)
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// buildLocalSegmentList renders one representation's segments, returning the
// list and the presentation time its last segment ends at.
func buildLocalSegmentList(segs []index.SegmentState, channelDir string) (localSegmentList, float64, error) {
	sl := localSegmentList{Timescale: segs[0].Timescale}

	if init := segs[0].InitPath; init != "" {
		rel, err := relativeToChannel(init, channelDir)
		if err != nil {
			return sl, 0, err
		}
		sl.Init = &localInit{SourceURL: rel}
	}

	timeline := &localTimeline{}
	var cur *localS
	for _, seg := range segs {
		rel, err := relativeToChannel(seg.Path, channelDir)
		if err != nil {
			return sl, 0, err
		}
		sl.URLs = append(sl.URLs, localSegURL{Media: rel})

		d := uint64(seg.EndPTS - seg.StartPTS)
		t := uint64(seg.StartPTS)
		switch {
		case cur == nil:
			timeline.S = append(timeline.S, localS{T: t, D: d})
			cur = &timeline.S[len(timeline.S)-1]
		case cur.D == d && cur.T+uint64(cur.R+1)*cur.D == t:
			cur.R++
		default:
			timeline.S = append(timeline.S, localS{T: t, D: d})
			cur = &timeline.S[len(timeline.S)-1]
		}
	}
	if len(timeline.S) > 0 {
		sl.Timeline = timeline
	}

	last := segs[len(segs)-1]
	return sl, last.EndSec(), nil
}

// relativeToChannel is store.RelativeToChannel, with the manifest's prefix on
// its errors.
func relativeToChannel(p, channelDir string) (string, error) {
	rel, err := store.RelativeToChannel(p, channelDir)
	if err != nil {
		return "", fmt.Errorf("local manifest: %w", err)
	}
	return rel, nil
}

// localSourceOffset is in seconds in the source presentation clock. A rep-only
// URL template inherits the AS offset, just as it inherits its timescale.
func localSourceOffset(rep *imdp.ParsedRepresentation, as *imdp.ParsedAdaptationSet) float64 {
	if t := rep.EffectiveTemplate(as); t != nil {
		offset := t.PresentationTimeOffset

		return float64(offset) / float64(max(t.Timescale, 1))
	}
	if l := rep.EffectiveSegmentList(as); l != nil {
		return float64(l.PresentationTimeOffset) / float64(max(l.Timescale, 1))
	}
	if b := rep.EffectiveSegmentBase(as); b != nil {
		return float64(b.PresentationTimeOffset) / float64(max(b.Timescale, 1))
	}
	return 0
}
