package channel

// NotRunning is the pseudo-field ApplyConfig reports when the channel has no
// runtime to update at all. It is not a config field: the caller must Start the
// channel rather than restart it, so callers that surface the blocking fields
// to a user should filter this one out.
const NotRunning = "not_running"

// RestartRequired reports which changed fields between old and next cannot be
// applied to a running channel, and therefore force a stop/start cycle. An
// empty result means the edit can be applied hot, with no interruption to
// ingest or playback.
//
// The rule is derived from what Start freezes into long-lived state: the ring
// buffer capacity, the choice of ingest driver, the key cache, and the source
// URL baked into the watcher. Everything else is either read per request by the
// HTTP handlers or, in the case of FetchHeaders, read through a provider that
// ApplyConfig can swap atomically.
//
// Restarting is not free — it re-scans the channel's segments from disk and
// resets the HLS media sequence — so a field belongs in this list only when
// hot-applying it would actually produce wrong behaviour.
func RestartRequired(old, next Config, mode ChannelMode) []string {
	// A VOD channel has no ingest goroutines at all: Start returns early once
	// the index is rebuilt. Nothing is watching an upstream, so the fields that
	// only ever configure ingest are inert and safe to swap. They still matter
	// for the *next* start, which is exactly what the store already records.
	//
	// This set must stay a subset of the live set below. Mode can change between
	// the check and the swap, and that is only harmless while relaxing is
	// strictly monotonic — a field hot in live but restarting in VOD would be a
	// real bug.
	vod := mode == ModeVOD

	var out []string
	add := func(changed bool, field string) {
		if changed {
			out = append(out, field)
		}
	}

	// Selects the ingest driver, the decrypt-on-ingest policy, whether the key
	// cache persists to disk, and the processor's publish mode. Wrong for a VOD
	// channel too: the segments already on disk were stored under the old
	// policy, so reinterpreting them live would break playback.
	add(old.canonicalSourceType() != next.canonicalSourceType(), "source_type")
	add(old.canonicalHLSKeyMode() != next.canonicalHLSKeyMode(), "hls_key_mode")

	// Frozen into the watcher at construction.
	add(!vod && old.MPDURL != next.MPDURL, "mpd_url")

	// Both size the segment ring buffer, which is allocated once per
	// representation and cannot be resized in place. A VOD channel's index is
	// already unbounded, so neither can shrink it.
	add(!vod && old.KeepAllSegments != next.KeepAllSegments, "keep_all_segments")
	add(!vod && old.EnableVODTransition != next.EnableVODTransition, "enable_vod_transition")

	// Decides the whole shape of the runtime, in either mode. The admin API
	// never lets a client set this, but the manager itself does on transition.
	add(old.IsVOD != next.IsVOD, "is_vod")

	return out
}
