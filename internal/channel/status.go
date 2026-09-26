package channel

// Channel status: what the manager reports about its configured channels.

import "github.com/a840817a/sluice/internal/index"

// ChannelInfo is one configured channel: its stored configuration and what it
// is doing now.
type ChannelInfo struct {
	Config Config        `json:"config"`
	Status ChannelStatus `json:"status"`
}

// Channels returns every configured channel, running or not, keyed by ID.
func (m *Manager) Channels() map[string]ChannelInfo {
	stored := m.store.List()
	m.mu.RLock()
	running := make(map[string]*Runtime, len(m.runtimes))
	for id, rt := range m.runtimes {
		running[id] = rt
	}
	m.mu.RUnlock()

	out := make(map[string]ChannelInfo, len(stored))
	for id, cfg := range stored {
		out[id] = ChannelInfo{Config: cfg, Status: channelStatus(id, cfg, running[id])}
	}
	return out
}

// HasChannel reports whether a channel with this ID is configured, running or
// not, without building its status.
func (m *Manager) HasChannel(id string) bool {
	_, ok := m.store.Get(id)
	return ok
}

// Channel returns one configured channel, running or not, and false if no
// channel has that ID. It builds only that channel's status, so a
// single-channel request does not pay for every other channel's per-track
// breakdown.
func (m *Manager) Channel(id string) (ChannelInfo, bool) {
	cfg, ok := m.store.Get(id)
	if !ok {
		return ChannelInfo{}, false
	}
	return ChannelInfo{Config: cfg, Status: channelStatus(id, cfg, m.Get(id))}, true
}

// channelStatus is the status of a configured channel: rt's, or a stopped one
// when rt is nil.
func channelStatus(id string, cfg Config, rt *Runtime) ChannelStatus {
	if rt == nil {
		return ChannelStatus{ID: id, MPDURL: cfg.MPDURL, Running: false}
	}
	return buildStatus(rt)
}

// ChannelStatus is a JSON-serialisable snapshot of a channel.
type ChannelStatus struct {
	ID      string        `json:"id"`
	MPDURL  string        `json:"mpd_url"`
	Running bool          `json:"running"`
	Mode    string        `json:"mode"`
	Periods []PeriodStats `json:"periods"`
	// Progress is what the channel has ingested since it was last started.
	// Absent for a channel that is not running.
	Progress *ProgressStats `json:"progress,omitempty"`
	// Tracks is the per-representation breakdown. Absent until the first segment
	// is indexed.
	Tracks []TrackStats `json:"tracks,omitempty"`
	// Source reports how close the origin is to being finished. A pointer so
	// a status with nothing to say serialises to the same keys it always had.
	Source *SourceStatus `json:"source_status,omitempty"`
	// VODWarnings lists restart-safety steps that failed while this channel
	// completed as VOD. Absent when there were none.
	VODWarnings []string `json:"vod_warnings,omitempty"`
}

// SourceStatus reports the origin's end-of-stream signal, which is what
// decides when a channel transitions to VOD: the MPD's presentation type for
// DASH, and the EXT-X-ENDLIST count across poll targets for HLS.
type SourceStatus struct {
	UpstreamType string `json:"upstream_type,omitempty"` // DASH: "dynamic" | "static"
	EndedTargets int    `json:"ended_targets,omitempty"` // HLS: playlists that reached ENDLIST
	PollTargets  int    `json:"poll_targets,omitempty"`  // HLS: playlists being polled
}

// PeriodStats summarises one period's segment counts.
type PeriodStats struct {
	PeriodID  string `json:"period_id"`
	Published int    `json:"published"`
	Committed int    `json:"committed"`
	Expired   int    `json:"expired"`
	// Total is the number of segments the index currently holds, which is always
	// exactly Published+Committed+Expired — the ring buffer it measures only grows
	// on commit. It is therefore useless as a download denominator, despite the
	// name: use ProgressStats.TotalSegments for that. Kept for compatibility.
	Total int `json:"total"`
}

// storedSegments counts the segments an index already tracks, in any status.
//
// This is the sum PeriodStats.Total reports, and the only correct use for it: as
// the count of what is on disk right now. It was never a meaningful denominator
// for a download bar, because the ring buffer it measures only grows when a
// segment is committed — so it always equalled the number already stored.
func storedSegments(ci *index.ChannelIndex) uint64 {
	var n uint64
	ci.ForEachRep(func(_ index.RepRef, rep *index.RepresentationState) {
		p, c, e := rep.StatusCounts()
		n += uint64(p + c + e)
	})
	return n
}

// sourceStatus reports what the channel's source knows about the origin
// ending, or nil when it knows nothing yet.
func sourceStatus(rt *Runtime) *SourceStatus {
	if rt.Source == nil {
		return nil
	}
	return rt.Source.Status()
}

func buildStatus(rt *Runtime) ChannelStatus {
	// One read for the whole status: the reported mode and the index the counts
	// come from must describe the same instant, or an operator sees "live" over
	// full-history numbers during a transition.
	sv := rt.Serving()

	s := ChannelStatus{
		ID:      rt.ID,
		MPDURL:  sv.Config.MPDURL,
		Running: true,
		Mode:    sv.Mode.String(),
		Source:  sourceStatus(rt),
	}
	ci := sv.Index
	// Layout includes a period with no AdaptationSets yet — serving a manifest
	// creates period state before any segment commits — which is reported
	// with zero counts.
	stats := make(map[string]*PeriodStats)
	for _, pl := range ci.Layout() {
		ps := &PeriodStats{PeriodID: pl.ID}
		for _, as := range pl.AdaptationSets {
			for _, rep := range as.Reps {
				ps.Published += rep.Published
				ps.Committed += rep.Committed
				ps.Expired += rep.Expired
			}
		}
		ps.Total = ps.Published + ps.Committed + ps.Expired
		stats[pl.ID] = ps
	}

	ids := make([]string, 0, len(stats))
	for pid := range stats {
		ids = append(ids, pid)
	}
	for _, pid := range sv.OrderPeriods(ids) {
		s.Periods = append(s.Periods, *stats[pid])
	}

	s.Progress = buildProgress(rt)
	s.Tracks = buildTracks(sv, ci)
	if w := rt.vodWarnings.Load(); w != nil {
		s.VODWarnings = *w
	}
	return s
}
