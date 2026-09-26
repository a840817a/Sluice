Shaka Player vendored asset

File:
- shaka-player.5.2.12.compiled.js
  sha256 b22dfdba631104c48a654cb623b08b0f79e2d52c83c0e9b4b7cff54730b55894

Source:
- https://cdn.jsdelivr.net/npm/shaka-player@5.2.12/dist/shaka-player.compiled.js
  (the npm package's build; ajax.googleapis.com does not host 5.2.12. For
  5.0.12 the two CDNs served byte-identical files.)

Project:
- https://github.com/shaka-project/shaka-player

Notes:
- Vendored to avoid runtime dependency on the external CDN.
- Everything under static/ is compiled into the gateway binary by go:embed and
  served publicly under /player-assets/, so only files actually referenced by a
  player page belong here.
- The v4.16.28 build was kept during the v5 upgrade for rollback and was removed
  on 2026-07-24 once both player pages had moved to 5.0.12. Re-download it from
  the same CDN path if a rollback is ever needed.
- Do not go back to 5.0.12 for HLS sources. Its TS transmuxer
  (H264.getVideoSamples) reads slice_type one byte too late, so a non-IDR
  I-frame that carries its own SPS and is coded as several slices can be split
  into two samples. Chrome then raises PIPELINE_ERROR_DECODE, and Shaka's
  MediaSource recovery restarts playback from 0 with a black screen.
  ExpGolomb.readSliceType is fixed in 5.2.12.
