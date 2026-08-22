package channel

import "context"

// Source drives ingestion for one channel: it discovers what the upstream is
// offering and pushes SegmentTasks onto the broker until it is done or the
// context is cancelled.
//
// It is declared here rather than in an ingest package because the consumer
// owns the interface: Manager is the only thing in the repo that stores or
// calls a Source, and both implementations — the DASH watcher and the HLS
// watcher — satisfy it structurally without importing it.
//
// The interface is deliberately just Run. Everything else the two
// implementations expose — the parsed MPD for DASH, the variant list for HLS —
// is protocol-specific and has no meaningful common shape, so callers reach for
// the concrete type they actually need instead of hiding the difference behind
// a lowest-common-denominator abstraction.
//
// Returning nil means ingestion completed (a static MPD, or an HLS playlist
// that reached EXT-X-ENDLIST); the channel manager then settles the channel
// into VOD. Returning a non-nil error triggers a backoff and restart.
type Source interface {
	Run(ctx context.Context) error
}
