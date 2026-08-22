Shaka Player vendored asset

File:
- shaka-player.5.0.12.compiled.js

Source:
- https://ajax.googleapis.com/ajax/libs/shaka-player/5.0.12/shaka-player.compiled.js

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
