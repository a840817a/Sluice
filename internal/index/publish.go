package index

// PublishPolicy decides when a committed segment becomes visible to players.
//
// It is a named type rather than a bool because the two rules are not "on and
// off" — they are two different correct answers, each wrong for the other
// protocol, and a bare true/false at the call site said nothing about which was
// which.
type PublishPolicy uint8

const (
	// PublishAVBarrier gates each segment on its period's A/V barrier, so audio
	// and video only become visible together. A DASH client is handed one
	// presentation with time-aligned tracks, so letting one track run ahead of
	// the other in the manifest produces a stream it cannot play cleanly.
	//
	// It is the zero value, so a policy left unset degrades to the stricter
	// rule rather than to no gating.
	PublishAVBarrier PublishPolicy = iota

	// PublishImmediately publishes each committed segment as soon as it lands.
	// HLS serves each track as its own media playlist and the client aligns
	// them itself, so gating one track behind another would only stall a
	// playlist whose peer momentarily lagged — or, when the index is rebuilt
	// from disk, drop the segments at a track's end that its peer never
	// matched.
	PublishImmediately
)
