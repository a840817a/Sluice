package channel

import (
	"context"
	"errors"

	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/presentation"
)

// tracks builds the channel's presentation.Tracks. parsed is the MPD the caller
// already read for the same Serving, so the two cannot disagree.
func (rt *Runtime) tracks(parsed *imdp.ParsedMPD) presentation.Tracks {
	if rt.hls != nil {
		d := rt.hls.Describe()
		return presentation.Tracks{
			Variants:            d.Variants,
			Renditions:          d.Renditions,
			Subtitles:           d.Subtitles,
			IndependentSegments: d.IndependentSegments,
			Ready:               d.Ready,
			ServesHLS:           true,
			ServesDASH:          d.FMP4,
		}
	}
	if parsed == nil {
		// Not fetched yet: servable both ways, nothing to serve.
		return presentation.Tracks{ServesHLS: true, ServesDASH: true}
	}
	return presentation.TracksFromMPD(parsed)
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
// key server URL is never taken from the request, which would make the gateway
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
