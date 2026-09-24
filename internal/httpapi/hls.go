package httpapi

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/go-chi/chi/v5"
)

const playlistContentType = "application/vnd.apple.mpegurl"

// handleMasterPlaylist serves the HLS multivariant playlist for a channel.
//
// GET /v1/channels/{channelID}/master.m3u8
func (s *Server) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.Error(w, "channel not found or not running", http.StatusNotFound)
		return
	}
	pr := rt.Serving().Presentation
	if !pr.ServesHLS {
		http.Error(w, "channel cannot be served as HLS (encrypted or unsupported)", http.StatusNotFound)
		return
	}
	if !pr.Ready {
		http.Error(w, "playlist not yet available", http.StatusServiceUnavailable)
		return
	}

	out := hlsout.Master(pr.Variants, hlsout.MasterOptions{
		GatewayBaseURL:      s.cfg.Server.BaseURL,
		ChannelID:           channelID,
		IndependentSegments: pr.IndependentSegments,
		Renditions:          pr.Renditions,
		Subtitles:           pr.Subtitles,
	})

	w.Header().Set("Content-Type", playlistContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// handleMediaPlaylist serves the HLS media playlist for one representation.
//
// GET /v1/channels/{channelID}/media/{repID}.m3u8
func (s *Server) handleMediaPlaylist(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	repID := chi.URLParam(r, "repID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.Error(w, "channel not found or not running", http.StatusNotFound)
		return
	}
	// One read of the channel for the whole response: the index the segments
	// come from and the VOD flag that describes them have to agree.
	sv := rt.Serving()
	if !sv.Presentation.ServesHLS {
		http.Error(w, "channel cannot be served as HLS", http.StatusNotFound)
		return
	}
	if !sv.Presentation.KnownRep(repID) {
		http.NotFound(w, r)
		return
	}

	periods := s.periodSegmentsForRep(sv, channelID, repID)
	var segs []index.SegmentState
	for _, p := range periods {
		segs = append(segs, p.Segments...)
	}

	// The advertised target duration must never shrink between reloads, so it
	// is tracked per channel+representation rather than recomputed per request.
	targetDur := s.stickyTargetDuration.Get(channelID+"/"+repID, segs)

	isVOD := sv.IsVOD()
	opts := hlsout.MediaOptions{
		GatewayBaseURL: s.cfg.Server.BaseURL,
		ChannelID:      channelID,
		RepID:          repID,
		VOD:            isVOD,
		TargetDuration: targetDur,
		WindowSegments: windowSegments(s.cfg.Window.Depth.Seconds(), targetDur, sv.Config.KeepsAllSegments() || isVOD),
	}
	// Segments kept encrypted on disk are advertised with a key URI pointing at
	// this gateway's proxy. Decrypt-on-ingest channels store cleartext and carry
	// no key material, so nothing is emitted for them.
	opts.KeyProxyBase = keyProxyURL(s.cfg.Server.BaseURL, channelID)

	out := hlsout.Media(periods, opts)

	w.Header().Set("Content-Type", playlistContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// synthesizeHLSManifest builds the stand-in DASH MPD for an fMP4 HLS channel,
// or nil when nothing is published yet. HLS carries no MPD, so the structure
// comes from the discovered variants and the timeline from the segment index.
//
// Demuxed audio renditions become a separate audio AdaptationSet, so the DASH
// output carries audio too (not just the video track).
func (s *Server) synthesizeHLSManifest(sv ch.Serving, snap index.Snapshot, isVOD bool, windowDepth time.Duration) *imdp.ParsedMPD {
	return manifest.SynthesizeHLSMPD(sv.Presentation.Variants, snap, manifest.HLSSynthOptions{
		ASID:      hlsingest.VideoASID,
		MediaType: imdp.MediaVideo,
		MimeType:  "video/mp4",
		Audio:     sv.Presentation.Renditions,
		AudioASID: hlsingest.AudioASID,
		VOD:       isVOD,
		// A stable anchor for the live edge; unused for VOD.
		AvailabilityStartTime: sv.StaticIngestStart,
		MinimumUpdatePeriod:   s.cfg.Upstream.PollInterval,
		TimeShiftBufferDepth:  windowDepth,
	})
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

// periodSegmentsForRep collects repID's Published segments period by period,
// in timeline order: a DASH source's periods in the order its MPD lists them,
// an HLS source's epochs numerically (index.OrderPeriodIDs). Merging them into
// one list would let two periods' identical segment numbers collide
// (ADR-0005).
func (s *Server) periodSegmentsForRep(sv ch.Serving, channelID, repID string) []hlsout.PeriodSegments {
	byPeriod := map[string]*hlsout.PeriodSegments{}
	var ids []string
	sv.Index.ForEachRep(func(ref index.RepRef, rep *index.RepresentationState) {
		if ref.RepID != repID {
			return
		}
		p, ok := byPeriod[ref.PeriodID]
		if !ok {
			p = &hlsout.PeriodSegments{PeriodID: ref.PeriodID}
			byPeriod[ref.PeriodID] = p
			ids = append(ids, ref.PeriodID)
		}
		p.Segments = append(p.Segments, rep.Published()...)
		if first, last, ok := rep.SegNoRange(); ok {
			p.First, p.Last, p.RangeKnown = first, last, true
		}
	})

	var docOrder []string
	if sv.ParsedMPD != nil {
		for _, p := range sv.ParsedMPD.Periods {
			docOrder = append(docOrder, p.ID)
		}
	}
	out := make([]hlsout.PeriodSegments, 0, len(ids))
	for _, id := range index.OrderPeriodIDs(ids, docOrder) {
		p := *byPeriod[id]
		// fMP4 representations need their initialization section advertised
		// via EXT-X-MAP; a non-empty init path is exactly the fMP4 signal, and
		// is empty for self-contained TS/WebVTT segments regardless of source.
		if publishedInitPath(sv.Index, id, repID) != "" {
			p.InitURI = gwurl.InitURL(s.cfg.Server.BaseURL, channelID, id, repID)
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

// handleKeyProxy serves an AES-128 content key to players on behalf of the
// upstream key server.
//
// GET /v1/channels/{channelID}/key/{keyID}
//
// keyID is the gateway's own identifier for a key URI, and is resolved through
// the registry of URIs actually seen in this channel's playlists. The upstream
// URL is deliberately never accepted from the request: taking it as a parameter
// would turn the gateway into an open proxy that any caller could aim at
// internal addresses.
func (s *Server) handleKeyProxy(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	keyID := chi.URLParam(r, "keyID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	// ResolveKey decides whether this channel brokers keys at all and whether
	// keyID is one it issued; both refusals look like an unknown key.
	key, err := rt.ResolveKey(r.Context(), keyID)
	if errors.Is(err, ch.ErrNoKeyProxy) || errors.Is(err, ch.ErrUnknownKey) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Warn("key proxy fetch failed", "channel", channelID, "err", err)
		http.Error(w, "key unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(key)))
	// Keys are per-key-period secrets brokered for a specific viewer; do not let
	// shared caches keep copies.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(key)
}
