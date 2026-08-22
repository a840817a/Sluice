package httpapi

import (
	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/hlsdesc"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Group IDs synthesized for demuxed tracks of a DASH source served as HLS.
const (
	dashAudioGroup = "audio"
)

// hlsView is everything the HLS master/media output needs, derived from either
// an HLS source or a clear fMP4 DASH source. Modelling both behind one shape is
// what lets a DASH channel serve HLS with the same handlers, so the two source
// protocols become mutually convertible for clear content.
type hlsView struct {
	variants            []hlsdesc.Variant
	renditions          []hlsdesc.Rendition
	subtitles           []hlsdesc.Subtitle
	independentSegments bool
	// ready is false when discovery has not produced representations yet.
	ready bool
}

// knownRep reports whether repID names a representation in this view.
func (v hlsView) knownRep(repID string) bool {
	for _, x := range v.variants {
		if x.RepID == repID {
			return true
		}
	}
	for _, x := range v.renditions {
		if x.RepID == repID {
			return true
		}
	}
	for _, x := range v.subtitles {
		if x.RepID == repID {
			return true
		}
	}
	return false
}

// hlsViewFor builds the HLS output description for a channel, returning ok=false
// when the channel cannot be served as HLS at all (an encrypted DASH source,
// whose HLS DRM signalling is out of scope). A capable-but-not-yet-ready channel
// returns a view with ready=false.
func (s *Server) hlsViewFor(rt *ch.Runtime) (hlsView, bool) {
	if rt.IsHLSSource() {
		return hlsView{
			variants:            rt.HLS.Variants(),
			renditions:          rt.HLS.Renditions(),
			subtitles:           rt.HLS.Subtitles(),
			independentSegments: rt.HLS.IndependentSegments(),
			ready:               rt.HLS.Ready(),
		}, true
	}

	// DASH source: derive the description from the upstream MPD. DASH is always
	// fMP4, so it maps cleanly onto fMP4 HLS output — for clear content.
	p := rt.ParsedMPD()
	if p == nil {
		return hlsView{ready: false}, true // not fetched yet
	}
	return dashHLSView(p)
}

// dashHLSView derives an HLS view from a parsed DASH manifest. Returns ok=false
// for an encrypted presentation, which cannot be re-signalled as HLS here.
func dashHLSView(p *imdp.ParsedMPD) (hlsView, bool) {
	for _, period := range p.Periods {
		for _, as := range period.AdaptationSets {
			if len(as.ContentProtections) > 0 || len(as.RawCPs) > 0 {
				return hlsView{}, false // encrypted: out of scope for HLS output
			}
		}
	}

	v := hlsView{ready: true}
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
					v.variants = append(v.variants, hlsdesc.Variant{
						RepID:     rep.ID,
						Bandwidth: rep.Bandwidth,
						Codecs:    rep.Codecs,
						Width:     int(rep.Width),
						Height:    int(rep.Height),
					})
				case imdp.MediaAudio:
					hasAudio = true
					v.renditions = append(v.renditions, hlsdesc.Rendition{
						RepID:    rep.ID,
						GroupID:  dashAudioGroup,
						Name:     dashRenditionName(as, rep),
						Language: as.Lang,
						Default:  len(v.renditions) == 0,
					})
					// DASH text AdaptationSets (fMP4-wrapped subtitles) are left
					// out: their HLS signalling is a separate concern.
				}
			}
		}
	}

	if len(v.variants) == 0 {
		return hlsView{ready: false}, true // nothing playable discovered yet
	}
	if hasAudio {
		for i := range v.variants {
			v.variants[i].AudioGroup = dashAudioGroup
		}
	}
	return v, true
}

// dashRenditionName gives an audio rendition a human name, preferring the
// AdaptationSet language.
func dashRenditionName(as *imdp.ParsedAdaptationSet, rep *imdp.ParsedRepresentation) string {
	if as.Lang != "" {
		return as.Lang
	}
	return rep.ID
}
