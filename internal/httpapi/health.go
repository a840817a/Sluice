package httpapi

// Per-channel health reporting.

import (
	"encoding/json"
	"net/http"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/go-chi/chi/v5"
)

// handleChannelHealth returns a JSON summary of segment counts for a channel.
//
// GET /v1/channels/{channelID}/health
func (s *Server) handleChannelHealth(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")

	type repHealth struct {
		Published int `json:"published"`
		Committed int `json:"committed"`
		Expired   int `json:"expired"`
	}
	type asHealth struct {
		MediaType string               `json:"media_type"`
		Reps      map[string]repHealth `json:"reps"`
	}
	type periodHealth struct {
		AdaptationSets map[string]asHealth `json:"adaptation_sets"`
	}
	type drmHealth struct {
		PlayReady bool `json:"playready"`
		Widevine  bool `json:"widevine"`
	}
	type healthResp struct {
		ChannelID string `json:"channel_id"`
		Title     string `json:"title,omitempty"`
		Running   bool   `json:"running"`
		// SourceType is the upstream protocol: "dash" or "hls".
		SourceType string `json:"source_type"`
		// ManifestURL is the URL clients should play. The gateway serves DASH
		// or HLS depending on the channel's source, so clients ask rather than
		// assume.
		ManifestURL string `json:"manifest_url"`
		// AltManifestURL is the other output format the channel also serves from
		// the same segments (HLS↔DASH), when available. Empty when the channel
		// serves only its native format (e.g. MPEG-TS HLS, or encrypted DASH).
		AltManifestURL string                   `json:"alt_manifest_url,omitempty"`
		DRM            drmHealth                `json:"drm"`
		Periods        map[string]*periodHealth `json:"periods,omitempty"`
	}

	mpdURL := gwurl.ManifestPath(channelID)
	m3u8URL := gwurl.MasterPath(channelID)

	resp := healthResp{
		ChannelID:   channelID,
		SourceType:  ch.SourceDASH,
		ManifestURL: mpdURL,
		DRM:         drmHealth{PlayReady: true, Widevine: true},
	}

	rt := s.manager.Get(channelID)
	if rt != nil {
		chCfg := rt.Config()
		resp.Running = true
		resp.Title = chCfg.Title
		if chCfg.IsHLS() {
			resp.SourceType = ch.SourceHLS
			resp.ManifestURL = m3u8URL
			// An fMP4 HLS channel additionally serves a DASH manifest.
			//
			// Both predicates are needed here. ServesDASH alone is true for a
			// DASH runtime too, and this branch is entered on the *config*
			// saying HLS, which does not by itself guarantee rt.HLS was built.
			if rt.IsHLSSource() && rt.ServesDASH() {
				resp.AltManifestURL = mpdURL
			}
		} else if view, ok := s.hlsViewFor(rt); ok && view.ready {
			// A clear DASH channel additionally serves HLS.
			resp.AltManifestURL = m3u8URL
		}
		resp.DRM = drmHealth{
			PlayReady: chCfg.PlayReadyEnabled(),
			Widevine:  chCfg.WidevineEnabled(),
		}
		resp.Periods = make(map[string]*periodHealth)

		rt.Index.Mu().RLock()
		for periodID, ps := range rt.Index.Periods {
			ph := &periodHealth{AdaptationSets: make(map[string]asHealth)}
			ps.Mu().RLock()
			for asID, as := range ps.AdaptationSets {
				ah := asHealth{
					MediaType: as.MediaType(),
					Reps:      make(map[string]repHealth),
				}
				as.Mu().RLock()
				for repID, rep := range as.Reps {
					pub, com, exp := rep.StatusCounts()
					ah.Reps[repID] = repHealth{
						Published: pub,
						Committed: com,
						Expired:   exp,
					}
				}
				as.Mu().RUnlock()
				ph.AdaptationSets[asID] = ah
			}
			ps.Mu().RUnlock()
			resp.Periods[periodID] = ph
		}
		rt.Index.Mu().RUnlock()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
