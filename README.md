# Sluice

A DASH and HLS live stream gateway that ingests from an upstream origin, caches segments locally, rewrites manifests and playlists with gateway URLs, and serves playback-ready streams to clients. Supports live-to-VOD transitions, multi-CDN fallback, DRM license proxying, AES-128 key brokering, and multi-channel management.

## Features

- **DASH and HLS sources** — A channel ingests either an upstream MPD or an HLS playlist, selected per channel with `source_type`
- **Live & VOD** — Serves dynamic (live) or static (VOD) output; auto-transitions when upstream ends
- **Multi-format ingestion** — DASH `SegmentTemplate`, `SegmentList`, and `SegmentBase`; HLS MPEG-TS, fMP4/CMAF, and WebVTT
- **No remuxing** — HLS segments are passed through byte-for-byte; `.ts` in, `.ts` out
- **Cross-protocol output** — Any clear fMP4 channel serves both `manifest.mpd` and `master.m3u8` from the same files on disk (see [Output formats](#output-formats))
- **AES-128 HLS** — Per-channel passthrough (ciphertext on disk, keys brokered by the gateway) or decrypt-on-ingest
- **A/V barrier** — Holds DASH segments until audio and video are temporally aligned before publishing
- **Sliding window** — Configurable `timeShiftBufferDepth` with safe-edge buffer
- **Multi-CDN fallback** — Rotates through `<BaseURL>` elements on fetch failure with exponential backoff
- **DRM proxying** — Transparent reverse proxy for PlayReady and Widevine license requests
- **Admin API** — REST API + embedded SPA for channel CRUD and real-time health
- **Prometheus metrics** — Segment throughput, queue depth, write latency
- **State persistence** — Channel state, HLS source description, segment timing, and AES keys survive restarts
- **Graceful shutdown** — HTTP drain completes before ingest workers stop

## Architecture

```
Upstream Origin (DASH MPD  |  HLS playlist)
        │
   [Watcher]  ← polls the manifest/playlist; DASH and HLS have
        │        separate watchers behind one ingest.Source seam
        │ discovers new segments
   [Broker]   ← priority task queue (dedup + deadline)
        │
   [Fetcher Pool] ← parallel HTTP downloads, retry + fallback
        │
   [Processor] ← validate (MP4 boxes / TS sync bytes / WEBVTT),
        │         optionally AES-128 decrypt, write to disk atomically
        │
   [A/V Barrier] ← hold until audio+video PTS overlap (DASH; HLS
        │           publishes tracks independently)
        │
   [Channel Index] ← in-memory sliding window of published segments
        │
   [Manifest Generator]  →  manifest.mpd
   [Playlist Generator]  →  master.m3u8 + media playlists
        │
   HTTP clients (browser players, CDN edges)
```

**Segment lifecycle:** Discovered → Fetching → Committed → Published → Expired

## Output formats

Output follows the source, except that fMP4 can be served as either protocol:

| Source | `manifest.mpd` | `master.m3u8` |
|--------|----------------|---------------|
| DASH, clear fMP4 | ✅ | ✅ |
| DASH, encrypted | ✅ | ❌ (HLS DRM signalling is out of scope) |
| HLS, fMP4/CMAF | ✅ | ✅ |
| HLS, MPEG-TS | ❌ (would require remuxing) | ✅ |

A channel's `health` endpoint advertises both its primary and, where available, its alternate manifest URL.

## Getting Started

### Build

```bash
go build -o sluice ./cmd/gateway
```

### Configure

Copy the example config and edit as needed:

```bash
cp config.yaml.example config.yaml
```

Minimum required fields:

```yaml
server:
  addr: ":8080"
  base_url: "http://your-gateway-host:8080"

admin:
  username: "admin"
  password: "changeme"

store:
  data_dir: "./data"
```

### Run

```bash
./sluice -config config.yaml
```

Channels are added at runtime via the Admin API — no restart required.

## Configuration Reference

```yaml
server:
  addr: ":8080"                    # Listening address
  base_url: "http://localhost:8080" # Public URL rewritten into manifest

admin:
  username: "admin"                # HTTP Basic Auth for /admin endpoints
  password: "changeme"

upstream:
  poll_interval: "2s"              # Manifest/playlist refresh interval
  auth_header: "Authorization"     # Optional auth header sent upstream
  auth_value: "Bearer <token>"

worker:
  fetch_workers: 4                 # Parallel download workers per channel
  max_retries_static: 5            # Max fetch retries (live and static alike)
  live_usefulness_ttl: "30s"       # Reserved; live fetches follow upstream DVR window
  max_segment_bytes: 134217728     # Per-segment memory ceiling; 0 = 128 MiB default

store:
  data_dir: "./data"               # Segment cache root directory
  cleanup: "disabled"              # disabled | on_expire
  enable_vod_transition: false     # Keep all segments for live→VOD transition

window:
  depth: "120s"                    # Sliding window size (timeShiftBufferDepth)
  safe_edge_buffer: "6s"           # Buffer behind live edge to prevent player race
```

The source URL is **not** a gateway-level setting — it is per channel (`mpd_url`), set through the Admin API. Per-channel config is persisted in `{data_dir}/channels.json`.

## API

### Public Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Health check |
| `GET` | `/metrics` | Prometheus metrics |
| `GET` | `/player/{channelID}` | Standalone browser player (no auth) |
| `GET` | `/v1/channels/{channelID}/manifest.mpd` | DASH manifest (rewritten) |
| `GET` | `/v1/channels/{channelID}/master.m3u8` | HLS multivariant playlist |
| `GET` | `/v1/channels/{channelID}/media/{repID}.m3u8` | HLS media playlist for one representation |
| `GET` | `/v1/channels/{channelID}/key/{keyID}` | AES-128 key proxy (passthrough HLS channels) |
| `GET` | `/v1/channels/{channelID}/health` | Channel segment state (JSON) |
| `GET` | `/v1/channels/{channelID}/segments/{repID}/{segFile}` | Media segment (`.m4s`, `.ts`, `.vtt`) |
| `GET` | `/v1/channels/{channelID}/init/{repID}.mp4` | Init segment (fMP4 only) |
| `POST` | `/v1/channels/{channelID}/license/playready` | PlayReady license proxy |
| `POST` | `/v1/channels/{channelID}/license/widevine` | Widevine license proxy |

Endpoints that do not apply to a channel return 404 — see [Output formats](#output-formats). CORS is enabled on all public endpoints.

`keyID` is the gateway's own identifier for an upstream key URI (a SHA-256 of it), resolved through a registry of URIs seen in that channel's own playlists. The upstream URL is never accepted as a request parameter, which would turn the gateway into an open proxy.

### Admin Endpoints (HTTP Basic Auth required)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/` | Admin SPA |
| `GET` | `/admin/api/channels` | List all channels |
| `POST` | `/admin/api/channels` | Create channel |
| `PUT` | `/admin/api/channels/{channelID}` | Update channel config |
| `DELETE` | `/admin/api/channels/{channelID}` | Stop and remove channel |
| `GET` | `/admin/api/channels/{channelID}/status` | Real-time ingest progress, per-track breakdown, and representation status |
| `POST` | `/admin/api/channels/{channelID}/transition-to-vod` | Trigger live→VOD transition |

#### Ingest Progress

Both `GET /admin/api/channels` and `.../{channelID}/status` carry a `progress` object
per running channel. Four of its fields have semantics that are easy to misread:

| Field | Meaning |
|-------|---------|
| `segments_stored` | Cumulative segments written **since the gateway last started this channel**. Not since creation: counters are not persisted, and a restart re-seeds this from what the disk rebuild restored. |
| `total_segments` / `total_known` | Only set for a bounded source (static MPD, HLS VOD). A live channel reports `total_known: false` and no total — it does not have one, so no percentage should be derived. `total_capped: true` means the value is a *floor* (upstream enumeration hit an internal cap), and `eta_seconds` is withheld. |
| `segments_failed` | Failed **attempts**. Most are retried and succeed, so a segment that needed three tries adds three here and still arrives. |
| `segments_dropped` | Work genuinely abandoned — expired, evicted under queue pressure, or out of retries. This, not `segments_failed`, is what explains a gap between `segments_stored` and `total_segments`. |

`source_error` is a live condition and clears on the next successful poll;
`last_segment_error` is sticky so a polling UI cannot miss it.

`tracks` gives the per-representation breakdown (codec, bitrate, resolution,
language, and segment counts). It is driven by the index rather than the manifest,
so a representation dropped upstream still appears while its segments are on disk.

#### Create Channel

```bash
curl -u admin:changeme -X POST http://localhost:8080/admin/api/channels \
  -H "Content-Type: application/json" \
  -d '{
    "id": "my-channel",
    "mpd_url": "https://origin.example.com/live/stream.mpd",
    "enabled": true
  }'
```

`mpd_url` holds the source URL for both protocols — for an HLS channel it is the playlist URL. The field keeps its original name so existing `channels.json` files stay valid.

Optional fields:

| Field | Description |
|-------|-------------|
| `title` | Display name in the admin UI |
| `source_type` | `dash` (default) or `hls` |
| `hls_key_mode` | `passthrough` (default) or `decrypt`; AES-128 HLS only |
| `playready_license_url`, `widevine_license_url` | Upstream license servers |
| `enable_playready`, `enable_widevine` | Both default to true |
| `fetch_headers` | `[{"name":…,"value":…}]` injected into upstream manifest/segment requests |
| `license_headers` | Same shape, injected into DRM license proxy requests |
| `forward_client_ip` | Pass `X-Forwarded-For` to the license server |
| `enable_vod_transition` | Keep all segments and auto-transition to VOD when upstream ends |
| `keep_all_segments` | Unbounded DVR window; stays live even when upstream goes static |

## Disk Layout

```
{data_dir}/
├── channels.json                              # Persistent channel configs
└── {channelID}/
    ├── manifest.mpd                           # Saved VOD manifest (after transition)
    ├── hls-source.json                        # HLS variants + key registry (HLS only)
    ├── keys.json                              # Cached AES-128 keys (passthrough only, 0600)
    └── periods/
        └── {periodID}/
            └── {mediaType}_{asID}/            # e.g. video_0, audio_1, text_2
                └── {repID}/
                    ├── init.mp4               # Init segment (fMP4 only)
                    ├── segments.jsonl         # Per-segment timing/key sidecar
                    └── {segNo}.m4s            # Media segments (.m4s | .ts | .vtt)
```

The sidecar exists because MPEG-TS carries no container timing: an HLS index cannot be
rebuilt from the segment files alone, so segment durations, discontinuities, and key
material are recorded alongside them.

## Observability

### Prometheus Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `mpd_segments_fetched_total` | Counter | `channel`, `status` | Fetch attempts by result (`ok`, `error`, `write_error`) |
| `mpd_segments_published_total` | Counter | `channel`, `media_type` | Segments that passed the A/V barrier |
| `mpd_broker_queue_depth` | Gauge | `channel` | Current task queue depth |
| `mpd_segment_write_duration_seconds` | Histogram | — | Atomic write latency |

Metrics are available at `GET /metrics` (no auth required; restrict via firewall in production).

### Structured Logging

All logs are JSON via `log/slog` to stdout. Key log events:

- `watcher stopped, restarting` — upstream unreachable, will retry with backoff
- `fetch error` — segment download failed, requeuing
- `segment validation failed` — invalid MP4 box structure, segment dropped
- `segment write failed, requeueing` — disk error, segment will retry

## Live → VOD Transition

When the upstream stream ends and the MPD changes from `type=dynamic` to `type=static`, the gateway:

1. Waits for the final MPD fetch
2. Rebuilds the channel index as an unbounded full history
3. Switches manifest output to a static (VOD) MPD
4. Sets `is_vod: true` in `channels.json`

On restart, channels with `is_vod: true` are restored as VOD directly — no re-ingestion needed.

To manually trigger (e.g., when upstream doesn't signal end-of-stream):

```bash
curl -u admin:changeme -X POST \
  http://localhost:8080/admin/api/channels/my-channel/transition-to-vod
```

## Security

- Admin endpoints are protected with HTTP Basic Auth
- `mpd_url` is validated to allow only `http://` and `https://` schemes
- DRM license requests are forwarded with configurable auth headers; client IP forwarding is opt-in
- The AES-128 key proxy resolves only key URIs seen in the channel's own playlists, never a URL supplied by the caller
- In passthrough mode the content key is cached on the gateway disk (`keys.json`, mode 0600) so playback survives the upstream key server going away. Content stays encrypted over the wire and to the CDN, but is no longer protected at rest on the gateway — use `decrypt` mode only where storing cleartext is acceptable

## Development

```bash
# Run all tests
go test ./...

# Run tests for a specific package
go test ./internal/httpapi/...
go test ./internal/index/...
go test ./internal/queue/...
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/unki2aut/go-mpd` | MPD XML parsing and marshaling |
| `github.com/abema/go-mp4` | MP4 box parsing (PTS extraction, validation) |
| `github.com/prometheus/client_golang` | Prometheus metrics |
| `github.com/google/uuid` | Channel and period identifiers |
| `gopkg.in/yaml.v3` | Configuration file parsing |

HLS playlist parsing and generation are implemented in-tree (`internal/hlssrc`, `internal/hlsout`) with no third-party dependency.
