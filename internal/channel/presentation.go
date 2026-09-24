package channel

import (
	"context"
	"errors"

	"github.com/a840817a/sluice/internal/hlsdesc"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Presentation is what a channel offers to players: its tracks, and which
// protocols it can be served as. It is built from the channel's source once
// per Serving read, so every output of one request — the master playlist, a
// synthesized DASH manifest, the status tracks, the health links — describes
// the same tracks and applies the same protocol rules (ADR-0002).
//
// Like Serving it is a value: it holds no live objects and does no I/O.
type Presentation struct {
	// Variants, Renditions and Subtitles describe the tracks in HLS terms,
	// whether they were discovered from an HLS source or derived from a DASH
	// source's MPD.
	Variants   []hlsdesc.Variant
	Renditions []hlsdesc.Rendition
	Subtitles  []hlsdesc.Subtitle
	// IndependentSegments mirrors the upstream EXT-X-INDEPENDENT-SEGMENTS tag.
	IndependentSegments bool
	// Ready is false until the source has discovered something playable.
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
func (p Presentation) KnownRep(repID string) bool {
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

// presentation builds the channel's Presentation. parsed is the MPD the caller
// already read for the same Serving, so the two cannot disagree.
func (rt *Runtime) presentation(parsed *imdp.ParsedMPD) Presentation {
	if rt.hls != nil {
		return Presentation{
			Variants:            rt.hls.Variants(),
			Renditions:          rt.hls.Renditions(),
			Subtitles:           rt.hls.Subtitles(),
			IndependentSegments: rt.hls.IndependentSegments(),
			Ready:               rt.hls.Ready(),
			ServesHLS:           true,
			ServesDASH:          rt.hls.IsFMP4(),
		}
	}
	if parsed == nil {
		// Not fetched yet: servable both ways, nothing to serve.
		return Presentation{ServesHLS: true, ServesDASH: true}
	}
	return dashPresentation(parsed)
}

// dashAudioGroup is the group ID given to the audio tracks of a DASH source
// served as HLS.
const dashAudioGroup = "audio"

// dashPresentation derives a Presentation from a parsed DASH manifest. DASH is
// always fMP4, so it maps cleanly onto fMP4 HLS output — for clear content; an
// encrypted presentation cannot be re-signalled as HLS here.
func dashPresentation(p *imdp.ParsedMPD) Presentation {
	for _, period := range p.Periods {
		for _, as := range period.AdaptationSets {
			if len(as.ContentProtections) > 0 || len(as.RawCPs) > 0 {
				return Presentation{ServesDASH: true, Ready: true}
			}
		}
	}

	pr := Presentation{ServesHLS: true, ServesDASH: true, Ready: true}
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
		return Presentation{ServesHLS: true, ServesDASH: true}
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

// Errors from ResolveKey.
var (
	// ErrNoKeyProxy: the channel brokers no keys — it is not an HLS source,
	// has no key cache, or decrypts on ingest and so stores cleartext.
	ErrNoKeyProxy = errors.New("channel does not serve keys")
	// ErrUnknownKey: the key ID was not seen in this channel's playlists.
	ErrUnknownKey = errors.New("unknown key ID")
)

// ResolveKey returns the AES-128 content key a player asks the key proxy for.
//
// It owns the whole rule. Only a passthrough HLS channel brokers keys: a
// decrypt-on-ingest channel republishes cleartext, and handing out its key
// would leak a credential for content already stripped of protection. The key
// ID must be one this channel issued for a URI in its own playlists — the
// upstream URL is never taken from the request, which would make the gateway
// an open proxy. Any other error is a failed fetch.
func (rt *Runtime) ResolveKey(ctx context.Context, keyID string) ([]byte, error) {
	if rt.hls == nil || rt.keys == nil || rt.Config().DecryptOnIngest() {
		return nil, ErrNoKeyProxy
	}
	uri, ok := rt.hls.KeyURIForID(keyID)
	if !ok {
		return nil, ErrUnknownKey
	}
	return rt.keys.Get(ctx, uri)
}
