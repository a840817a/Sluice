# Output protocol follows the source protocol

A channel serves the protocol its source uses: an HLS source is served as HLS playlists, a DASH source as a DASH manifest. A channel serves both only when its segments are fMP4/CMAF and clear, because only then can one set of files on disk back both protocols without transmuxing (ADR-0001). DASH output for such an HLS source is synthesized into the existing manifest generator rather than written by a second one. A DASH channel whose AdaptationSets are all clear, whether originally or after decryption (ADR-0010), serves HLS; one encrypted AdaptationSet keeps HLS closed.

## Out of scope

- FairPlay, and HLS DRM signalling for encrypted DASH sources (a DASH channel with an AdaptationSet still encrypted does not serve HLS).
- Remuxing MPEG-TS sources to DASH.
- Multi-CDN failover for HLS sources: multi-CDN origins are rare for the deployments this serves.
