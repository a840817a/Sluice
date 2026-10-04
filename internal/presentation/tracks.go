package presentation

import (
	"github.com/a840817a/sluice/internal/hlsdesc"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Tracks is what a channel offers to players: its tracks, and which
// protocols it can be served as. It is built from the channel's source once
// per Serving read, so every output of one request — the master playlist, a
// synthesized DASH manifest, the status tracks, the health links — describes
// the same tracks and applies the same protocol rules (ADR-0002).
//
// Like Serving it is a value: it holds no live objects and does no I/O.
type Tracks struct {
	// Variants, Renditions and Subtitles describe the tracks in HLS terms,
	// whether they were discovered from an HLS source or derived from a DASH
	// source's MPD.
	Variants   []hlsdesc.Variant
	Renditions []hlsdesc.Rendition
	Subtitles  []hlsdesc.Subtitle
	// IndependentSegments mirrors the origin's EXT-X-INDEPENDENT-SEGMENTS tag.
	IndependentSegments bool
	// Ready is false until the tracks above describe something playable. An
	// encrypted DASH source, which lists no tracks in HLS terms, is never
	// Ready; it is served as DASH regardless.
	Ready bool
	// ServesHLS reports whether the channel can be served as HLS: an HLS
	// source always, a DASH source only when its content is clear, because
	// HLS DRM signalling for a DASH source is out of scope.
	ServesHLS bool
	// ServesDASH reports whether the channel can produce a DASH manifest: a
	// DASH source always, an HLS source only when its segments are fMP4,
	// because MPEG-TS would need remuxing (ADR-0001).
	ServesDASH bool
}

// KnownRep reports whether repID names one of the presentation's tracks.
func (p Tracks) KnownRep(repID string) bool {
	for _, x := range p.Variants {
		if x.RepID == repID {
			return true
		}
	}
	for _, x := range p.Renditions {
		if x.RepID == repID {
			return true
		}
	}
	for _, x := range p.Subtitles {
		if x.RepID == repID {
			return true
		}
	}
	return false
}

// dashAudioGroup is the group ID given to the audio tracks of a DASH source
// served as HLS.
const dashAudioGroup = "audio"

// TracksFromMPD derives the Tracks of a DASH source from its parsed manifest.
// DASH is always fMP4, so it maps cleanly onto fMP4 HLS output — for clear
// content; an encrypted presentation cannot be re-signalled as HLS here.
func TracksFromMPD(p *imdp.ParsedMPD) Tracks {
	for _, period := range p.Periods {
		for _, as := range period.AdaptationSets {
			if len(as.ContentProtections) > 0 || len(as.RawCPs) > 0 {
				return Tracks{ServesDASH: true}
			}
		}
	}

	pr := Tracks{ServesHLS: true, ServesDASH: true, Ready: true}
	seen := map[string]bool{}
	hasAudio := false

	for _, period := range p.Periods {
		for _, as := range period.AdaptationSets {
			for _, rep := range as.Representations {
				if seen[rep.ID] {
					continue // representations repeat across periods
				}
				seen[rep.ID] = true

				switch as.MediaType {
				case imdp.MediaVideo:
					pr.Variants = append(pr.Variants, hlsdesc.Variant{
						RepID:     rep.ID,
						Bandwidth: rep.Bandwidth,
						Codecs:    rep.Codecs,
						Width:     int(rep.Width),
						Height:    int(rep.Height),
					})
				case imdp.MediaAudio:
					hasAudio = true
					pr.Renditions = append(pr.Renditions, hlsdesc.Rendition{
						RepID:    rep.ID,
						GroupID:  dashAudioGroup,
						Name:     dashRenditionName(as, rep),
						Language: as.Lang,
						Default:  len(pr.Renditions) == 0,
					})
					// DASH text AdaptationSets (fMP4-wrapped subtitles) are left
					// out: their HLS signalling is a separate concern.
				}
			}
		}
	}

	if len(pr.Variants) == 0 {
		// Nothing playable discovered yet.
		return Tracks{ServesHLS: true, ServesDASH: true}
	}
	if hasAudio {
		for i := range pr.Variants {
			pr.Variants[i].AudioGroup = dashAudioGroup
		}
	}
	return pr
}

// dashRenditionName gives an audio rendition a human name, preferring the
// AdaptationSet language.
func dashRenditionName(as *imdp.ParsedAdaptationSet, rep *imdp.ParsedRepresentation) string {
	if as.Lang != "" {
		return as.Lang
	}
	return rep.ID
}
