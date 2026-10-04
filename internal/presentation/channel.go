// Package presentation renders what a channel offers to players: its DASH
// manifest and its HLS playlists, and which of them it can serve at all.
package presentation

import (
	"time"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Channel is everything output generation needs to know about one channel at
// one instant. It is a value read once per request (channel.Serving holds it),
// so every output of that request describes the same instant: a channel
// finishing into VOD midway through cannot yield a manifest that declares
// itself live while its segments come from the full-history index.
//
// It carries facts, not the lifecycle: VOD and StaticIngesting are what the
// channel mode means for output, and the configuration is reduced to the
// switches output reads.
type Channel struct {
	// Index is the index that describes what the channel can serve: the
	// full-history VOD index once a transition has completed, the live index
	// otherwise. Callers must not reapply that rule — resolving it here is the
	// reason this field exists.
	Index *index.ChannelIndex
	// Tracks is what the channel offers to players: its tracks, and whether it
	// can be served as HLS and as DASH.
	Tracks Tracks
	// ParsedMPD is the most recently fetched origin MPD, nil for HLS channels.
	ParsedMPD *imdp.ParsedMPD
	// IsHLSSource reports whether the channel ingests HLS. It reflects the
	// runtime that was built, not the configuration that asked for it.
	IsHLSSource bool
	// ConfigIsHLS reports whether the configuration asks for an HLS source,
	// which can briefly disagree with IsHLSSource.
	ConfigIsHLS bool
	// VOD reports that the channel has completed its transition to VOD.
	VOD bool
	// StaticIngesting reports that a static origin has been discovered and its
	// segments are still downloading. Such a channel is not yet VOD and is
	// deliberately still served from the live index.
	StaticIngesting bool
	// KeepAll reports that the channel keeps its full segment history.
	KeepAll bool
	// PlayReady and Widevine report which DRM systems the channel signals.
	PlayReady, Widevine bool
	// FinalDuration is the mediaPresentationDuration the origin announced in
	// its final MPD, or 0 if it announced none.
	FinalDuration time.Duration
	// StaticIngestStart is when a static origin was first detected, or the
	// zero time if that has not happened.
	StaticIngestStart time.Time
}

// OrderPeriods returns period IDs in timeline order: a DASH source's periods in
// the order its MPD lists them, an HLS source's periods numerically
// (index.OrderPeriodIDs).
func (c Channel) OrderPeriods(ids []string) []string {
	var docOrder []string
	if c.ParsedMPD != nil {
		for _, p := range c.ParsedMPD.Periods {
			docOrder = append(docOrder, p.ID)
		}
	}
	return index.OrderPeriodIDs(ids, docOrder)
}
