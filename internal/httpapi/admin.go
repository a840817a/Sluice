package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// AdminServer holds dependencies for admin API handlers.
type AdminServer struct {
	manager *ch.Manager
	ctx     context.Context // parent context for starting channels
}

// NewAdminServer creates an AdminServer.
func NewAdminServer(ctx context.Context, manager *ch.Manager) *AdminServer {
	return &AdminServer{manager: manager, ctx: ctx}
}

// handleListChannels returns all configured channels with their running status.
//
// GET /admin/api/channels
func (a *AdminServer) handleListChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.manager.Channels())
}

// handleCreateChannel creates and starts a new channel.
//
// POST /admin/api/channels
func (a *AdminServer) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	type request struct {
		ID string `json:"id"`
		ch.Config
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ID == "" {
		req.ID = uuid.New().String()
	}
	if err := req.Config.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Config.Enabled = true
	if err := a.manager.Create(a.ctx, req.ID, req.Config); err != nil {
		switch {
		case errors.Is(err, ch.ErrInvalidChannelID):
			writeErr(w, http.StatusBadRequest, "id must match [a-zA-Z0-9_-]{1,64}")
		case errors.Is(err, ch.ErrChannelExists):
			writeErr(w, http.StatusConflict, "channel already exists")
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": req.ID, "title": req.Config.Title})
}

// updateResponse tells the caller whether the edit interrupted the channel, so
// the admin UI can report what actually happened rather than always claiming a
// restart.
type updateResponse struct {
	ID        string `json:"id"`
	Restarted bool   `json:"restarted"`
	// RestartFields lists the edited fields that forced the restart. Empty when
	// nothing was restarted, and when the channel simply was not running.
	RestartFields []string `json:"restart_fields,omitempty"`
}

// handleUpdateChannel replaces the config for an existing channel.
//
// Most edits are applied to the running channel in place. Only fields that Start
// freezes into long-lived state (see channel.RestartRequired) force a stop and
// start, because restarting re-scans segments from disk and resets the HLS media
// sequence.
//
// PUT /admin/api/channels/{channelID}
func (a *AdminServer) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	// An unknown channel is a 404 whatever the body says. UpdateConfig checks
	// again under its operation lock, so a delete racing this request still
	// ends in 404 rather than recreating the channel.
	if !a.manager.HasChannel(id) {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	var cfg ch.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := a.manager.UpdateConfig(a.ctx, id, cfg)
	if err != nil {
		if errors.Is(err, ch.ErrChannelNotFound) {
			writeErr(w, http.StatusNotFound, "channel not found")
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, updateResponse{
		ID: id, Restarted: result.Restarted,
		RestartFields: result.RestartFields,
	})
}

// handleRestartChannel stops and starts a channel with its stored config,
// without changing anything. It gives an operator a way to force the re-scan
// and reconnect that an edit no longer performs on its own.
//
// POST /admin/api/channels/{channelID}/restart
func (a *AdminServer) handleRestartChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	if err := a.manager.Restart(a.ctx, id); err != nil {
		if errors.Is(err, ch.ErrChannelNotFound) {
			writeErr(w, http.StatusNotFound, "channel not found")
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, updateResponse{ID: id, Restarted: true})
}

// handleDeleteChannel removes a channel (does not delete disk data).
//
// DELETE /admin/api/channels/{channelID}
func (a *AdminServer) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	if err := a.manager.Delete(id); err != nil {
		if errors.Is(err, ch.ErrChannelNotFound) {
			writeErr(w, http.StatusNotFound, "channel not found")
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChannelStatus returns real-time stats for one channel.
//
// GET /admin/api/channels/{channelID}/status
func (a *AdminServer) handleChannelStatus(w http.ResponseWriter, r *http.Request) {
	info, ok := a.manager.Channel(chi.URLParam(r, "channelID"))
	if !ok {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	writeJSON(w, http.StatusOK, info.Status)
}

// handleTransitionToVOD manually triggers a live→VOD transition for a channel.
// Used when the origin does not emit a type=static MPD signal.
//
// POST /admin/api/channels/{channelID}/transition-to-vod
func (a *AdminServer) handleTransitionToVOD(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	if err := a.manager.TransitionToVOD(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":     id,
		"status": "transitioning",
	})
}

// handleRestoreToLive brings a VOD channel back to live ingest, undoing an
// automatic transition that fired on an origin hiccup.
//
// POST /admin/api/channels/{channelID}/restore-to-live
func (a *AdminServer) handleRestoreToLive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	// a.ctx, never the request's: the channel outlives the response, and a
	// request-scoped context would cancel its ingest the moment this returns.
	if err := a.manager.RestoreToLive(a.ctx, id); err != nil {
		if errors.Is(err, ch.ErrChannelNotFound) {
			writeErr(w, http.StatusNotFound, "channel not found")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":     id,
		"status": "live",
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
