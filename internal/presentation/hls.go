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

	periods := g.periodSegments(ch, repID)
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

// periodSegments collects repID's Published segments period by period, in
// timeline order (Channel.OrderPeriods). Merging them into one list would let
// two periods' identical segment numbers collide (ADR-0005).
func (g *Generator) periodSegments(ch Channel, repID string) []hlsout.PeriodSegments {
	reads := ch.Index.RepresentationPeriods(repID)
	byPeriod := make(map[string]index.PeriodRead, len(reads))
	ids := make([]string, 0, len(reads))
	for _, p := range reads {
		byPeriod[p.PeriodID] = p
		ids = append(ids, p.PeriodID)
	}
	out := make([]hlsout.PeriodSegments, 0, len(reads))
	for _, id := range ch.OrderPeriods(ids) {
		r := byPeriod[id]
		p := hlsout.PeriodSegments{
			PeriodID: id, Segments: r.Segments,
			First: r.First, Last: r.Last, RangeKnown: r.RangeKnown,
			Discontinuities: r.Discontinuities,
		}
		// A non-empty init path identifies fMP4; TS/WebVTT are self-contained.
		if r.InitPath != "" {
			p.InitURI = gwurl.InitURL(g.baseURL, ch.ID, id, repID)
		}
		out = append(out, p)
	}
	return out
}

// keyProxyURL returns the base URL of a channel's AES-128 key proxy.
//
// A thin wrapper over gwurl so that urls_test.go's byte-exact cases — including
// the trailing-slash ones — keep guarding the shape from this side.
func keyProxyURL(baseURL, channelID string) string {
	return gwurl.KeyProxyBaseURL(baseURL, channelID)
}
