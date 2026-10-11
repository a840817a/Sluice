package presentation

import (
	"errors"
	"slices"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Errors from the renderers, matched with errors.Is. The error a renderer
// returns reads as the reason a player or an operator should see.
var (
	// ErrNotServed: the channel cannot produce this output at all.
	ErrNotServed = errors.New("output not served")
	// ErrNotReady: the channel can produce this output, but has nothing to
	// put in it yet.
	ErrNotReady = errors.New("output not ready")
)

// unavailable is an ErrNotServed or ErrNotReady carrying its reason.
type unavailable struct {
	kind   error
	reason string
}

func (e *unavailable) Error() string { return e.reason }
func (e *unavailable) Unwrap() error { return e.kind }

func notServed(reason string) error { return &unavailable{ErrNotServed, reason} }
func notReady(reason string) error  { return &unavailable{ErrNotReady, reason} }

// Generator renders channels' DASH manifests and HLS playlists under one
// gateway configuration. It is safe for concurrent use.
type Generator struct {
	baseURL        string
	windowDepth    time.Duration
	safeEdgeBuffer time.Duration
	pollInterval   time.Duration
	// targetDuration keeps each HLS representation's advertised
	// EXT-X-TARGETDURATION monotonically non-decreasing across reloads. Keys
	// are "{channelID}/{repID}".
	targetDuration hlsout.StickyTargetDuration
}

// NewGenerator returns a Generator for the gateway configured by cfg.
func NewGenerator(cfg config.Config) *Generator {
	return &Generator{
		baseURL:        cfg.Server.BaseURL,
		windowDepth:    cfg.Window.Depth,
		safeEdgeBuffer: cfg.Window.SafeEdgeBuffer,
		pollInterval:   cfg.Upstream.PollInterval,
	}
}

// DASHNow renders ch's DASH manifest from a snapshot of its index taken now.
func (g *Generator) DASHNow(ch Channel) ([]byte, error) {
	// Synthesis decides which representations exist and at what timescale, and
	// generation builds their timelines; reading the index twice let those two
	// answers come from two different instants while ingest kept committing in
	// between.
	return g.DASH(ch, ch.Index.Snapshot())
}

// DASH renders ch's DASH manifest from snap, a snapshot of ch.Index. A caller
// that writes other artifacts from the same snapshot passes it here so that
// they all describe the same segments.
//
// A DASH source has a manifest only once its origin MPD has been fetched; an
// fMP4 HLS source has one only once a segment is published, because its MPD is
// synthesized from the discovered variants and the index (ADR-0002).
func (g *Generator) DASH(ch Channel, snap index.Snapshot) ([]byte, error) {
	if err := servesDASH(ch); err != nil {
		return nil, err
	}
	windowDepth := g.windowDepth
	if ch.KeepAll {
		windowDepth = 24 * time.Hour
	}

	p := ch.ParsedMPD
	if ch.IsHLSSource {
		p = g.synthesizeHLSMPD(ch, snap, windowDepth)
	}
	if p == nil {
		if ch.VOD && !ch.IsHLSSource && len(ch.SavedDASH) > 0 {
			return slices.Clone(ch.SavedDASH), nil
		}
		return nil, notReady("manifest not yet available")
	}

	cfg := manifest.GeneratorConfig{
		GatewayBaseURL:              g.baseURL,
		ChannelID:                   ch.ID,
		LicenseURL:                  licenseURL(g.baseURL, ch.ID, "playready", ch.PlayReady),
		WidevineLicenseURL:          licenseURL(g.baseURL, ch.ID, "widevine", ch.Widevine),
		DisablePlayReady:            !ch.PlayReady,
		DisableWidevine:             !ch.Widevine,
		WindowDepth:                 windowDepth,
		SafeEdgeBuffer:              g.safeEdgeBuffer,
		FallbackMinimumUpdatePeriod: g.pollInterval,
		VODMode:                     ch.VOD,
		FinalDuration:               ch.FinalDuration,
		StaticIngesting:             ch.StaticIngesting,
		StaticIngestStart:           ch.StaticIngestStart,
	}
	return manifest.Generate(p, snap, cfg)
}

// synthesizeHLSMPD builds the stand-in DASH MPD for an fMP4 HLS channel, or nil
// when nothing is published yet. HLS carries no MPD, so the structure comes
// from the discovered variants and the timeline from the segment index.
//
// Demuxed audio renditions become a separate audio AdaptationSet, so the DASH
// output carries audio too (not just the video track).
func (g *Generator) synthesizeHLSMPD(ch Channel, snap index.Snapshot, windowDepth time.Duration) *imdp.ParsedMPD {
	return manifest.SynthesizeHLSMPD(ch.Tracks.Variants, snap, manifest.HLSSynthOptions{
		ASID:      hlsingest.VideoASID,
		MediaType: imdp.MediaVideo,
		MimeType:  "video/mp4",
		Audio:     ch.Tracks.Renditions,
		AudioASID: hlsingest.AudioASID,
		VOD:       ch.VOD,
		// A stable anchor for the live edge; unused for VOD.
		AvailabilityStartTime: ch.StaticIngestStart,
		MinimumUpdatePeriod:   g.pollInterval,
		TimeShiftBufferDepth:  windowDepth,
	})
}

// licenseURL returns the URL of a channel's DRM license proxy, or "" when
// that DRM system is disabled for the channel.
func licenseURL(baseURL, channelID, drmType string, enabled bool) string {
	if !enabled {
		return ""
	}
	return gwurl.LicenseURL(baseURL, channelID, drmType)
}
