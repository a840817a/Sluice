package presentation

import "github.com/a840817a/sluice/internal/gwurl"

// Offerings says where a channel's outputs are: the manifest or playlist a
// player should open, and the other format the channel also serves from the
// same segments, when it does. Both are gateway-relative paths.
type Offerings struct {
	// Native follows the configured source protocol (ADR-0002): master.m3u8
	// for an HLS source, manifest.mpd for a DASH source.
	Native string
	// Alt is the other format, or "" when the channel serves only its native
	// one (an MPEG-TS HLS source, an encrypted DASH source).
	Alt string
}

// Offerings reports where ch's outputs are. It applies the rules the
// renderers refuse by, so it never advertises an output they would refuse as
// not served.
func (g *Generator) Offerings(ch Channel) Offerings {
	mpd, m3u8 := gwurl.ManifestPath(ch.ID), gwurl.MasterPath(ch.ID)
	if ch.ConfigIsHLS {
		o := Offerings{Native: m3u8}
		// An fMP4 HLS channel additionally serves a DASH manifest. Whether DASH
		// is served alone is not enough: it is for a DASH runtime too, and the
		// configuration saying HLS does not by itself guarantee an HLS runtime.
		if ch.IsHLSSource && servesDASH(ch) == nil {
			o.Alt = mpd
		}
		return o
	}
	o := Offerings{Native: mpd}
	// A clear DASH channel additionally serves HLS, once its tracks are known.
	if servesMaster(ch) == nil {
		o.Alt = m3u8
	}
	return o
}

// servesDASH reports whether ch can produce a DASH manifest at all: a DASH
// source always, an HLS source only when its segments are fMP4, because
// MPEG-TS would need remuxing (ADR-0001).
func servesDASH(ch Channel) error {
	if !ch.Tracks.ServesDASH {
		return notServed("channel has an MPEG-TS HLS source; use master.m3u8")
	}
	return nil
}

// servesMaster reports whether ch has a multivariant playlist to give: it must
// be servable as HLS, and its tracks must be known.
func servesMaster(ch Channel) error {
	if !ch.Tracks.ServesHLS {
		return notServed("channel cannot be served as HLS (encrypted or unsupported)")
	}
	if !ch.Tracks.Ready {
		return notReady("playlist not yet available")
	}
	return nil
}
