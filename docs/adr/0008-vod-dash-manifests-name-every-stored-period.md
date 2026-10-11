# A DASH VOD names every period the gateway stored

A DASH manifest is generated from the origin's MPD: it takes each Period's AdaptationSets and representations from there and its segments from the index. The VOD transition used the last MPD the origin served, which lists only what the origin still offered at the end. A period that had rolled off that MPD, or the gateway period an origin restart ended (ADR-0007), stayed on disk and in the VOD index but was left out of the served `manifest.mpd`, `local.mpd`, and the manifest a restarted gateway serves. A DASH player of the VOD lost that part of the asset, while the HLS playlists, which list periods from the index, kept it.

The DASH watcher now saves the origin MPD that lists each gateway period in that period's directory, as `periods/{period}/origin.mpd`. It writes the copy when it first sees the period and again whenever the origin changes which AdaptationSets and representations the period lists. At the VOD transition, and when a VOD is restored, every period the VOD index holds that the last MPD does not list is described from its copy and put back in timeline order (`index.OrderPeriodIDs`). We chose a copy per period over keeping every MPD in memory, because a VOD restored after a gateway restart has to describe the periods too, and over the description of a neighbouring period, because a period that rolled off can carry other representations than the ones that followed it.

In a multi-period VOD each period starts at the start its origin gave it, or where the period before it ends if that is later, and `mediaPresentationDuration` is where the last one ends. A period an origin restart started usually carries the same start as the one it ended, and periods described by different MPDs have no common timeline otherwise.

## Consequences

- A period stored before these copies existed borrows the description of the last MPD's period with the same origin Period ID, which covers a restarted stream. A period with neither is left out of the DASH manifests and named in `vod_warnings` (ADR-0004).
- A live DASH manifest still lists only the periods of the origin's current MPD (ADR-0007).
- A single-period VOD keeps its origin start and its index-scanned duration.
- `upstream.mpd` stays the raw last MPD; the periods are rebuilt from the copies on every restore.
