package presentation

import (
	"math"

	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
)

// Master renders ch's HLS multivariant playlist.
func (g *Generator) Master(ch Channel) ([]byte, error) {
	if err := servesMaster(ch); err != nil {
		return nil, err
	}
	return hlsout.Master(ch.Tracks.Variants, hlsout.MasterOptions{
		GatewayBaseURL:      g.baseURL,
		ChannelID:           ch.ID,
		IndependentSegments: ch.Tracks.IndependentSegments,
		Renditions:          ch.Tracks.Renditions,
		Subtitles:           ch.Tracks.Subtitles,
	}), nil
}

// Media renders the HLS media playlist of ch's representation repID.
//
// The advertised target duration never shrinks between reloads of the same
// playlist, so the Generator remembers the largest it has advertised for each
// channel and representation.
func (g *Generator) Media(ch Channel, repID string) ([]byte, error) {
	if !ch.Tracks.ServesHLS {
		return nil, notServed("channel cannot be served as HLS")
	}
	if !ch.Tracks.KnownRep(repID) {
		// net/http's NotFound text, which this answer has always carried.
		return nil, notServed("404 page not found")
	}

	periods := ch.Index.Timeline(repID, ch.PeriodDocOrder())
	var segs []index.SegmentState
	for _, p := range periods {
		segs = append(segs, p.Segments...)
	}
	targetDur := g.targetDuration.Get(ch.ID+"/"+repID, segs)

	return hlsout.Media(periods, hlsout.MediaOptions{
		GatewayBaseURL: g.baseURL,
		ChannelID:      ch.ID,
		RepID:          repID,
		VOD:            ch.VOD,
		TargetDuration: targetDur,
		WindowSegments: windowSegments(g.windowDepth.Seconds(), targetDur, ch.KeepAll || ch.VOD),
		// Segments kept encrypted on disk are advertised with a key URI
		// pointing at this gateway's proxy. Decrypt-on-ingest channels store
		// cleartext and carry no key material, so nothing is emitted for them.
		KeyProxyBase: keyProxyURL(g.baseURL, ch.ID),
	}), nil
}

// windowSegments converts the configured sliding-window depth into a segment
// count. Returns 0 (no cap) when the channel keeps its full history.
func windowSegments(depthSec float64, targetDur int, keepAll bool) int {
	if keepAll || depthSec <= 0 || targetDur <= 0 {
		return 0
	}
	n := int(math.Ceil(depthSec / float64(targetDur)))
	// RFC 8216 §6.2.2 requires a live playlist to hold at least three segments.
	if n < 3 {
		n = 3
	}
	return n
}

// keyProxyURL returns the base URL of a channel's AES-128 key proxy.
//
// A thin wrapper over gwurl so that urls_test.go's byte-exact cases — including
// the trailing-slash ones — keep guarding the shape from this side.
func keyProxyURL(baseURL, channelID string) string {
	return gwurl.KeyProxyBaseURL(baseURL, channelID)
}
