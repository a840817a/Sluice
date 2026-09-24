package channel

import (
	"time"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// Serving is what a channel is offering to clients at one instant: the mode it
// is in, the index that describes what can be served, which protocols it can
// produce, and the timing anchors output generation needs.
//
// It is a value, not a handle. It carries no live objects — no watcher, no key
// cache, no broker — and performs no I/O. Callers that need those keep reaching
// for them on the Runtime; what belongs here is state that can be copied and
// then reasoned about without worrying that it moved underneath.
//
// This is a different thing from index.Snapshot, which freezes the *contents*
// of one index. Serving freezes the answer to "which index, and in what mode" —
// the segments inside Index keep arriving. A handler that needs both takes a
// Serving and then calls Snapshot on its Index.
//
// The point of collecting these together is that they are read as a group and
// have to agree. Read one at a time, a channel finishing into VOD midway
// through a request yields a mix: a manifest that declares itself live while
// its segments come from the full-history index. Serving reads every field
// once, so a caller either sees the whole channel before the transition or the
// whole channel after it.
type Serving struct {
	// Mode is the channel's lifecycle state at the moment of the read.
	Mode ChannelMode
	// Index is the index that describes what the channel can serve: the
	// full-history VOD index once a transition has completed, the live index
	// otherwise. Callers must not reapply that rule — resolving it here is the
	// reason this field exists.
	Index *index.ChannelIndex
	// Config is the channel's configuration, copied by value.
	Config Config
	// IsHLSSource reports whether the channel ingests HLS. It reflects the
	// runtime that was built, not the configuration that asked for it.
	IsHLSSource bool
	// Presentation is what the channel offers to players: its tracks, and
	// whether it can be served as HLS and as DASH.
	Presentation Presentation
	// FinalDuration is the mediaPresentationDuration the upstream announced in
	// its final MPD, or 0 if it announced none.
	FinalDuration time.Duration
	// StaticIngestStart is when a static upstream was first detected, or the
	// zero time if that has not happened.
	StaticIngestStart time.Time
	// ParsedMPD is the most recently fetched upstream MPD, nil for HLS channels.
	ParsedMPD *imdp.ParsedMPD
}

// IsVOD reports whether the channel has completed its transition to VOD.
func (s Serving) IsVOD() bool { return s.Mode == ModeVOD }

// IsStaticIngesting reports whether a static upstream has been discovered and
// its segments are still downloading. Such a channel is not yet VOD and is
// deliberately still served from the live index.
func (s Serving) IsStaticIngesting() bool { return s.Mode == ModeStaticIngesting }

// Serving returns what the channel is offering right now.
//
// It reads each underlying atomic exactly once. Callers that need several of
// these facts must take one Serving and read from it, rather than calling the
// individual accessors in sequence: that is what keeps the facts consistent
// with one another.
//
// It is cheap by design — a handful of atomic loads and a config copy, no index
// snapshot and no I/O — so that no caller has a reason to reach past it.
func (rt *Runtime) Serving() Serving {
	mode := rt.Mode()

	// Resolved from the mode read above rather than by calling ActiveIndex,
	// which would load the mode a second time and reopen the very window this
	// type exists to close.
	ci := rt.Index
	if mode == ModeVOD {
		if vod := rt.VODIndex(); vod != nil {
			ci = vod
		}
	}

	parsed := rt.ParsedMPD()
	return Serving{
		Mode:              mode,
		Index:             ci,
		Config:            rt.Config(),
		IsHLSSource:       rt.IsHLSSource(),
		Presentation:      rt.presentation(parsed),
		FinalDuration:     rt.FinalDuration(),
		StaticIngestStart: rt.StaticIngestStartTime(),
		ParsedMPD:         parsed,
	}
}
