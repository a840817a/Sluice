package channel

import (
	"context"
	"fmt"

	"github.com/a840817a/sluice/internal/dashingest"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Source drives ingestion for one channel: it discovers what the origin is
// offering and pushes SegmentTasks onto the broker until it is done or the
// context is cancelled. It also answers the questions the channel's lifecycle
// asks about the origin, so the Manager never needs to know which protocol
// it is ingesting.
//
// It is declared here rather than in an ingest package because the consumer
// owns the interface. dashSource and hlsSource adapt the two watchers to it;
// the watchers themselves do not import this package.
//
// What a channel is serving — its tracks, its parsed MPD, whether it can be
// served as DASH — is not the Source's business: that is a description of the
// presentation, and callers still reach the concrete watcher for it.
type Source interface {
	// Run ingests until the origin is done or ctx ends. Returning nil means
	// ingestion completed (a static MPD, or HLS playlists that reached
	// EXT-X-ENDLIST); the Manager then settles the channel into VOD. A non-nil
	// error triggers a backoff and restart.
	Run(ctx context.Context) error
	// FinishedAsset reports whether the origin has said it is a completed
	// asset rather than a live stream. It can be true well before Run returns.
	FinishedAsset() bool
	// Status reports what the source knows about the origin ending, or nil
	// when it knows nothing yet.
	Status() *SourceStatus
	// PersistsSegmentMeta reports whether each stored segment has a sidecar
	// record, which the VOD transition repairs from the index before the
	// rebuild reads it (ADR-0001).
	PersistsSegmentMeta() bool
	// Checkpoint makes the source's own state restart-safe once ingest has
	// stopped. snapshot is the rebuilt VOD index. Every failure is returned as
	// a warning (ADR-0004). It may go to the network; ctx bounds it.
	Checkpoint(ctx context.Context, snapshot index.Snapshot) []string
}

// dashSource adapts the DASH watcher. A DASH channel's restart safety is the
// MPD and the index, so it has nothing of its own to checkpoint.
type dashSource struct{ w *dashingest.Watcher }

func (s dashSource) Run(ctx context.Context) error { return s.w.Run(ctx) }

// FinishedAsset is the manifest's presentation type.
func (s dashSource) FinishedAsset() bool {
	return s.w.UpstreamType() == string(imdp.PresentationStatic)
}

// Status is nil before the first MPD fetch.
func (s dashSource) Status() *SourceStatus {
	if t := s.w.UpstreamType(); t != "" {
		return &SourceStatus{UpstreamType: t}
	}
	return nil
}

// PersistsSegmentMeta is false: fMP4 segments carry their own timing.
func (dashSource) PersistsSegmentMeta() bool { return false }

func (dashSource) Checkpoint(context.Context, index.Snapshot) []string { return nil }

// hlsSource adapts the HLS watcher and the channel's key cache, whose content
// keys are part of what a passthrough HLS channel must keep to play after a
// restart (ADR-0003). keys is nil when there is no cache to checkpoint.
type hlsSource struct {
	w    *hlsingest.Watcher
	keys *hlskey.Cache
}

func (s hlsSource) Run(ctx context.Context) error { return s.w.Run(ctx) }

// FinishedAsset is every polled playlist carrying EXT-X-ENDLIST on its first
// poll.
func (s hlsSource) FinishedAsset() bool { return s.w.OriginIsVOD() }

// Status is nil before discovery has named the playlists to poll.
func (s hlsSource) Status() *SourceStatus {
	if ended, total := s.w.EndListProgress(); total > 0 {
		return &SourceStatus{EndedTargets: ended, PollTargets: total}
	}
	return nil
}

// PersistsSegmentMeta is true: MPEG-TS has no tfdt, so timing, discontinuities
// and key material live in each segment's sidecar record.
func (hlsSource) PersistsSegmentMeta() bool { return true }

func (s hlsSource) Checkpoint(ctx context.Context, snapshot index.Snapshot) []string {
	var warnings []string
	if err := s.w.Checkpoint(); err != nil {
		warnings = append(warnings, fmt.Sprintf("checkpoint HLS source state: %v", err))
	}
	if s.keys == nil {
		return warnings
	}
	if err := s.keys.Checkpoint(ctx, requiredKeyURIs(snapshot)); err != nil {
		warnings = append(warnings, fmt.Sprintf("checkpoint HLS keys: %v", err))
	}
	return warnings
}
