package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var validChannelID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// AdminServer holds dependencies for admin API handlers.
type AdminServer struct {
	manager *ch.Manager
	store   *ch.Store
	ctx     context.Context // parent context for starting channels
}

// NewAdminServer creates an AdminServer.
func NewAdminServer(ctx context.Context, manager *ch.Manager, store *ch.Store) *AdminServer {
	return &AdminServer{manager: manager, store: store, ctx: ctx}
}

// handleListChannels returns all configured channels with their running status.
//
// GET /admin/api/channels
func (a *AdminServer) handleListChannels(w http.ResponseWriter, r *http.Request) {
	stored := a.store.List()
	running := a.manager.Status()

	out := make(map[string]interface{}, len(stored))
	for id, cfg := range stored {
		status, ok := running[id]
		if !ok {
			status = ch.ChannelStatus{ID: id, MPDURL: cfg.MPDURL, Running: false}
		}
		out[id] = map[string]interface{}{
			"config": cfg,
			"status": status,
		}
	}
	writeJSON(w, http.StatusOK, out)
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
	} else if !validChannelID.MatchString(req.ID) {
		writeErr(w, http.StatusBadRequest, "id must match [a-zA-Z0-9_-]{1,64}")
		return
	}
	if err := req.Config.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, exists := a.store.Get(req.ID); exists {
		writeErr(w, http.StatusConflict, "channel already exists")
		return
	}
	req.Config.Enabled = true
	if err := a.store.Put(req.ID, req.Config); err != nil {
		writeErr(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if _, err := a.manager.Start(a.ctx, req.ID, req.Config); err != nil {
		writeErr(w, http.StatusInternalServerError, "start error: "+err.Error())
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
	oldCfg, exists := a.store.Get(id)
	if !exists {
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

	// Fields the client does not get to set. Applied before the restart decision
	// so that a value the client could not have changed is never mistaken for an
	// edit that needs a restart.
	cfg.Enabled = true
	cfg.IsVOD = oldCfg.IsVOD

	if blockers := a.manager.ApplyConfig(id, cfg); len(blockers) == 0 {
		// Applied to the running channel already; persist it so a gateway
		// restart keeps the change.
		if err := a.store.Put(id, cfg); err != nil {
			// Put the runtime back to what is actually on disk, so the running
			// channel and the store never disagree.
			_ = a.manager.ApplyConfig(id, oldCfg)
			writeErr(w, http.StatusInternalServerError, "store error: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updateResponse{ID: id, Restarted: false})
		return
	} else if err := a.manager.RestartWithConfig(a.ctx, id, cfg, oldCfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else {
		writeJSON(w, http.StatusOK, updateResponse{
			ID:            id,
			Restarted:     true,
			RestartFields: userFacingRestartFields(blockers),
		})
	}
}

// handleRestartChannel stops and starts a channel with its stored config,
// without changing anything. It gives an operator a way to force the re-scan
// and reconnect that an edit no longer performs on its own.
//
// POST /admin/api/channels/{channelID}/restart
func (a *AdminServer) handleRestartChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	cfg, exists := a.store.Get(id)
	if !exists {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	_ = a.manager.Stop(id)
	if _, err := a.manager.Start(a.ctx, id, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "start error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updateResponse{ID: id, Restarted: true})
}

// userFacingRestartFields drops the not-running sentinel, which names no field
// the operator edited: the channel was simply started rather than restarted.
func userFacingRestartFields(blockers []string) []string {
	out := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if b != ch.NotRunning {
			out = append(out, b)
		}
	}
	return out
}

// handleDeleteChannel removes a channel (does not delete disk data).
//
// DELETE /admin/api/channels/{channelID}
func (a *AdminServer) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	if _, exists := a.store.Get(id); !exists {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	_ = a.manager.Stop(id)
	if err := a.store.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChannelStatus returns real-time stats for one channel.
//
// GET /admin/api/channels/{channelID}/status
func (a *AdminServer) handleChannelStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channelID")
	rt := a.manager.Get(id)
	if rt == nil {
		cfg, ok := a.store.Get(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "channel not found")
			return
		}
		writeJSON(w, http.StatusOK, ch.ChannelStatus{
			ID:      id,
			MPDURL:  cfg.MPDURL,
			Running: false,
		})
		return
	}
	status, ok := a.manager.StatusFor(id)
	if !ok {
		// Stopped between the Get above and here.
		writeJSON(w, http.StatusOK, ch.ChannelStatus{ID: id, Running: false})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleTransitionToVOD manually triggers a live→VOD transition for a channel.
// Used when the upstream does not emit a type=static MPD signal.
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

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
