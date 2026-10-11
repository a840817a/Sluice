package channel

import (
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/presentation"
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
	// Config is the channel's configuration, copied by value.
	Config Config
	// Channel is what output generation reads, derived from the fields above
	// and the runtime in the same read.
	presentation.Channel
}

// IsVOD reports whether the channel has completed its transition to VOD.
func (s Serving) IsVOD() bool { return s.Mode == ModeVOD }

// IsStaticIngesting reports whether a static origin has been discovered and
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
// It does no I/O and takes no index snapshot, so that no caller has a reason
// to reach past it. It does build the Tracks, which copies the channel's
// track lists; a caller that needs only the config reads Runtime.Config.
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

	return rt.servingAs(mode, ci)
}

// servingAs reads the channel as Serving does, but in the given mode and
// serving from ci.
func (rt *Runtime) servingAs(mode ChannelMode, ci *index.ChannelIndex) Serving {
	cfg := rt.Config()
	parsed := rt.ParsedMPD()
	return Serving{
		Mode:   mode,
		Config: cfg,
		Channel: presentation.Channel{
			ID:                rt.ID,
			Index:             ci,
			Tracks:            rt.tracks(parsed),
			ParsedMPD:         parsed,
			IsHLSSource:       rt.IsHLSSource(),
			ConfigIsHLS:       cfg.IsHLS(),
			VOD:               mode == ModeVOD,
			StaticIngesting:   mode == ModeStaticIngesting,
			KeepAll:           cfg.KeepsAllSegments(),
			PlayReady:         cfg.PlayReadyEnabled(),
			Widevine:          cfg.WidevineEnabled(),
			FinalDuration:     rt.FinalDuration(),
			StaticIngestStart: rt.StaticIngestStartTime(),
			SavedDASH:         rt.savedDASHManifest(),
		},
	}
}
