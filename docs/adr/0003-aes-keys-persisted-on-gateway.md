# Passthrough AES-128 keys are persisted on the gateway

In passthrough key mode, the gateway captures each content key during ingest, while the origin's key server is still reachable, and stores it in the channel's `keys.json` (mode 0600). This lets a channel keep playing, including after a gateway restart, when the origin's key server is gone. We accepted that keys now sit on the gateway's disk beside the ciphertext, so content at rest on the gateway is no longer protected; the goal of passthrough is that content stays encrypted over the wire and in CDN caches, and that still holds. Decrypt-mode channels keep keys in memory only.

## Consequences

- Keys must be captured during ingest, not on a player's first request, which may come after the origin is gone.
- The data volume holds key material and must be treated as sensitive, not as a disposable cache.
