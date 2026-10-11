# A DASH origin that restarts its numbering starts a gateway period

A period starts when the origin restarts its numbering (CONTEXT.md). An HLS source shows this as its media sequence going backwards. A DASH origin can restart inside the same Period ID, for example an encoder restart that republishes Period "p0" with `startNumber` back at 1. The gateway used to take a DASH period's identity from the MPD's Period ID alone, so the restarted stream went into the period that held the old one. Lower numbers were backfilled as holes, higher ones were appended with timestamps running backwards, and the playlists spliced the two streams together. Segment URLs that name their period (ADR-0005) do not help when both streams share one.

The DASH watcher now renames each origin Period to the gateway period its segments go to. A dynamic origin period has restarted when the gateway holds or has queued segments in its current gateway period and either:

- one of its representations lists a highest segment number below the highest the gateway holds or has queued for it, because a live edge never moves backwards; or
- the MPD's `availabilityStartTime` changed since the last MPD the watcher processed.

The gateway then starts `{origin period ID}.r{n}`: "p0.r1" for the first restart of "p0", "p0.r2" for the next. We chose a suffix over the HLS scheme of numbered periods so that an unrestarted DASH period keeps its origin ID, and with it its stored directory and URLs. The name uses only characters `pathseg` leaves as they are (ADR-0006). Period order places `X.rN` after `X`, in ascending N.

An origin period the watcher sees for the first time maps to its latest generation in the index. A gateway restart, and a VOD restore, therefore resume the period they left off in, and nothing new is persisted.

## Consequences

- A restart that keeps its numbering is caught only by the `availabilityStartTime` check, and only while the gateway runs: the previous value is not persisted. The same holds for a restart whose new numbering has already passed the old live edge by the time the gateway sees it, which needs the gateway to have been down for that long.
- An origin whose own Period ID ends in `.r` and a number, such as "p0.r1", is read as a restart of "p0". No origin is known to name periods this way.
- A live DASH manifest lists only the periods of the origin's current MPD, so after a restart it serves the newest generation. A VOD names every period the gateway stored (ADR-0008). The HLS playlist lists every period the gateway holds, in order, with a discontinuity between them.
- Static MPDs are never treated as restarted.
