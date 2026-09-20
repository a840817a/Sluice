# Local (filesystem-relative) manifest

**Status:** implemented 2026-09-21. Two details below changed during
implementation and are recorded as built, not as first sketched.

## Problem

A finished channel's on-disk `manifest.mpd` addresses segments the way the
gateway serves them — `<BaseURL>/</BaseURL>` plus
`v1/channels/{id}/segments/{rep}/$Number%09d$.m4s`. Nothing at those paths
exists on disk, so a tool pointed at the data directory cannot read the asset.
The paths that do exist are

```
{dataDir}/{channelID}/periods/{periodID}/{mediaType}_{asID}/{repID}/{segNo}.m4s
{dataDir}/{channelID}/periods/{periodID}/{mediaType}_{asID}/{repID}/init.mp4
```

with **unpadded** segment numbers (`1535.m4s`, not `000001535.m4s`) and the
init segment inside the representation's own directory.

`manifest.mpd` keeps its current shape: a restart restores a channel from it
(`savedVODManifestPath`), and a downstream program merges it. This design adds
a sibling; it changes nothing about the existing file.

## Decisions

| Question | Decision |
|---|---|
| When written | At VOD completion, in the same `OnVODReady` callback that writes `manifest.mpd` |
| Format | Follows the source: DASH → `local.mpd`, HLS → `local.m3u8` |
| Segment addressing | Explicit per-segment listing, so a hole in the numbering cannot misaddress anything |

Holes are the normal case, not a corner: a live channel sheds segments, and a
static ingest can leave stragglers far past its contiguous run. A `$Number$`
template cannot express a gap in *numbering* — only in time — so anything
template-shaped would silently misname segments after the first hole.

## Layout

```
{dataDir}/{channelID}/manifest.mpd          unchanged — served shape, restore source
{dataDir}/{channelID}/local.mpd             new, DASH sources
{dataDir}/{channelID}/local.m3u8            new, HLS sources — master
{dataDir}/{channelID}/local_{N}.m3u8        new, HLS sources — one per representation
```

No `<BaseURL>` element: relative paths then resolve against the manifest's own
directory, which is what a reader opening the file from disk does.

## DASH output

Each `Representation` carries a `<SegmentList>` holding `<SegmentTimeline>`
(exact per-segment durations — audio alternates 95232/96256 ticks, so a single
nominal `@duration` would drift over hundreds of segments), an
`<Initialization sourceURL="..."/>`, and one `<SegmentURL media="..."/>` per
published segment.

`SegmentTimeline` inside `SegmentList` is spec-legal — `SegmentList` extends
`MultipleSegmentBaseInformation` — but tool support varies. If the downstream
merger rejects it, the fallback is `SegmentList@duration` with its accumulating
error; that is a one-line change.

### Why a separate writer

`go-mpd`, the library that marshals the served manifest, has no `SegmentList`
type at all — which is why `buildRepFromSegmentList` converts a SegmentList
*source* into a SegmentTemplate *output*. The local manifest therefore gets its
own small `encoding/xml` writer covering only the elements it needs, rather
than contorting the served generator. The served path is left untouched, so it
cannot regress. ContentProtection is carried over from `ParsedAdaptationSet.RawCPs`, the same
material the served manifest injects.

`RawCPs` is documented as "the verbatim XML string ... including
namespace-prefixed attributes and children that encoding/xml drops", and it is
not: it round-trips through encoding/xml, so `cenc:default_KID` comes out as
`xmlns:_="urn:mpeg:cenc:2013" _:default_KID` and `<cenc:pssh>` as
`<pssh xmlns="urn:mpeg:cenc:2013">`. Those are namespace-equivalent and a
namespace-aware reader resolves them identically, but a consumer matching on
literal prefixes will not see what it expects. This is pre-existing and shows
up in the served manifest too; the local manifest reproduces it rather than
diverging from its sibling. Worth fixing at the parser, out of scope here.

## HLS output

A master playlist listing each representation, and one media playlist per
representation listing segments explicitly, with `#EXT-X-MAP` for fMP4 sources.
A break in segment numbering is marked `#EXT-X-DISCONTINUITY`: a gap is a gap in
the timeline, and saying so is what stops a reader treating the jump as
continuous media.

Media playlists are numbered (`local_0.m3u8`) rather than named after the
representation, and sit beside the master rather than in a subdirectory. Both
for the same reason: a representation ID must never become a path, and one
relativisation rule — everything relative to the channel directory — is one
fewer thing to get wrong than two.

`SaveVODManifest` returned early for HLS channels, because it begins by
requiring a parsed MPD and an HLS channel has none — so HLS sources have never
had a `manifest.mpd` snapshot at all. The local-playlist branch therefore runs
*before* that check; hanging it off the end of the existing function would have
made it unreachable for exactly the sources it was written for.
Verified against ffmpeg 9.0.1: a local playlist of fMP4 segments with
deliberately non-contiguous filenames read back as `format_name=hls`,
`duration=12.000000`, 360 packets for 12s at 30fps — every segment read.

## Error handling

Failure to write the local manifest logs a warning and nothing more. Serving
and restart-restore are the main line; this is an additional artifact and must
never cost the channel its `manifest.mpd`.

## Testing

The test that matters is not "XML was produced" but **every path the manifest
references exists on disk**: build a channel directory, generate, resolve each
reference relative to the manifest, and stat it.

Plus:
- Non-contiguous segment numbers are all listed, none renumbered.
- A guard against the generated paths drifting from `store.SegmentPath`, whose
  layout they must match — a divergence there would silently produce dangling
  references.
- The existing served-manifest tests must still pass unchanged.

## Note on ffmpeg

The `.mpd` half cannot be verified end-to-end on this machine: the local
ffmpeg 9.0.1 build has no DASH demuxer (no libxml2 — `-f dash` reports
"Unknown input format", and ffmpeg's own dash-muxer output fails to read back
too). The `.mpd` is validated at the XML/spec level and by the path-existence
test; its real consumer is the downstream merging program.
