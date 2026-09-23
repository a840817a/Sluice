# HLS segments pass through byte-for-byte; no transmuxing

Sluice stores and serves every HLS segment exactly as the origin produced it. An MPEG-TS source stays MPEG-TS and is never remuxed into fMP4, so it can only be served as HLS. We chose this over transmuxing so the gateway never has to decode or rewrite media, which keeps segment handling cheap, byte-identical to the origin, and free of a codec dependency.

## Consequences

- MPEG-TS has no tfdt box, so an HLS index cannot be rebuilt from segment files alone. Segment timing, discontinuities, and key material are persisted beside the segments (`segments.jsonl`, `hls-source.json`) for restart and the VOD transition to work.
- Validation of encrypted segments can only check length: a sync-byte check on ciphertext rejects every valid segment.
