# Testing procedures

Procedures too long for `AGENTS.md`. Each one exists because the obvious
approach gave a wrong answer here.

## Checking a multi-step refactor

Commit after each step. Before the first step, record three things, and diff
them after every step:

- the import graph: `go list -f '{{.ImportPath}} {{join .Imports " "}}' ./... | sort`
- the exported API: `for p in $(go list ./...); do go doc -all $p; done`
- the set of tests that ran, which catches a test whose subject a rename
  orphaned, so it silently stopped running:

```
go test ./... -count=1 -json | grep '"Action":"pass"' \
  | sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"]*)".*/\1\t\2/' \
  | awk -F'\t' 'NF==2' | sort -u
```

`comm -23 before.txt after.txt` must print nothing. Keep the `awk NF==2`
filter: package-level events have no test name and carry a timestamp, so
without it every run reports phantom lost tests.

A regex rename also rewrites alias declarations and embedded-field selectors,
and a blind `ingest.` → `dashingest.` replace turns `hlsingest.` into
`hlsdashingest.`. Let `go build` list the fallout, and run gofmt: renames leave
import order and field alignment unformatted.

## Looking at the admin UI

The admin UI sits behind basic auth, and an agent's browser must not enter a
password. Serve `internal/web/static` with a small static server on another
port that also answers `GET /admin/api/channels` with a hand-written
`{id: {config, status}}` payload covering every channel mode, plus a
`progress.source_error` for the degraded state, then open `/admin/`.

The page does not render while `document.hidden` is true, which it is in a
background tab. If it stays on 「載入中…」, run on the page:

```js
Object.defineProperty(document, 'hidden', {configurable: true, get: () => false});
Object.defineProperty(document, 'visibilityState', {configurable: true, get: () => 'visible'});
document.dispatchEvent(new Event('visibilitychange'));
```

and wait about three seconds for the next poll. Check states through the DOM
rather than screenshots, and find forms by id (`addForm`, `editForm`).

## Looking at the player

`player.html` has no JS tests. To check it by hand, serve a stub rather than a
gateway: answer `/player/{id}` with `internal/web/static/player.html`,
`/player-assets/*` from `internal/web/static/`, and
`/v1/channels/{id}/health` with `{"manifest_url": ..., "drm": {"playready":
false, "widevine": false}}` pointing at an HLS stream from ffmpeg (`-f lavfi
-i testsrc2` plus `sine`; add `-re` and `-hls_flags delete_segments` for a
live one). The player's functions are globals, so drive states from the page
(`reconnect(...)`, `applyModeConfig()`).

To time a start, poll until `video.currentTime` advances. `playing` is no
measure: Shaka buffers by setting the playback rate to 0, so `playing` fires
within milliseconds whatever the buffering goal.

Picture-in-picture and fullscreen need a real user gesture: press the key or
click the button through the browser tool, then script the rest. The built-in
browser pane never enters fullscreen, and the PiP window's own ✕ is outside
the page, so fake a close as `video.pause()` next to
`document.exitPictureInPicture()`.

## Reproducing a race in channel Stop

"Stop mid-ingest, then assert no segment file appears" passes even when Stop
does not wait for its goroutines: segment fetches observe cancellation, and the
window after it is microseconds. `TestStopLeavesNoWritesBehind` is kept only as
an upper bound.

What reproduces a non-draining Stop is work that does not observe the channel's
context. `TestStopCancelsInFlightKeyFetch` (`internal/channel/drain_test.go`)
is the pattern: an origin whose key handler blocks until its request is
cancelled, a passthrough channel, Stop once the key request arrives, and an
assertion that cancellation arrives well inside the fetch timeout.

When changing Stop, enumerate every `go` statement in `internal/channel`:
each goroutine must be counted in the runtime's WaitGroup, and Stop must
release the manager lock before waiting, or every HTTP handler stalls.

## Fixtures and flakes

- The DASH origin fixtures (`startClearDASHOrigin` and friends) exist twice, in
  `internal/httpapi` and `cmd/gateway`, because test files cannot be imported.
  A wire-format change must update both.
- Segments publish out of order. After waiting for a playlist, request a
  segment it actually lists (`firstSegmentFile`), never a fixed number.
- `http.FileServer` redirects `/admin/index.html` to `/admin/`; request the
  directory in router tests.
