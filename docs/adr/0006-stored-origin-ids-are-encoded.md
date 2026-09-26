# Stored origin IDs are encoded

A segment is stored at `{data}/{channel}/periods/{period}/{mediaType}_{as}/{representation}/`, and the period and representation IDs of a DASH source are whatever the origin's MPD says. Before this they were joined into the path raw. A period with no `id` attribute has the empty ID, which the path join drops, so its files landed one level too high; the rebuild from disk then read the AdaptationSet directory as the period and rebuilt nothing, and the channel entered VOD with empty playlists. An ID containing `/` or `..` placed files outside the data directory, which let whoever controls an origin's MPD write anywhere the gateway can. Each ID is now written as one directory name in the encoding the segment URLs already use (ADR-0005): letters, digits, `.`, `_` and `-` as is, anything else — including the empty ID, `.` and `..` — as `~` and unpadded base64url. Both live in `internal/pathseg`, so a URL and a directory can never encode the same ID differently. We chose encoding over rejecting unsafe IDs, because the empty ID is legal DASH and would have needed a substitute ID anyway.

## Consequences

- IDs made of the plain characters keep the directory they always had, so stored channels of every HLS source and of most DASH sources are unaffected.
- The rebuild reads a directory name that does not decode as the raw ID it was written under, so a channel stored with, say, a `:` in a period ID still rebuilds after the upgrade. New segments of that period go to the encoded directory beside it.
- Nothing is migrated. A period with no `id` stored before the upgrade stays unreadable, and files an ID already placed outside the data directory stay where they are.
- An ID longer than about 190 bytes encodes to more than the 255-byte file name limit, and its segments fail to store.
