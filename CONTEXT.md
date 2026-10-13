# Sluice

A gateway that ingests DASH and HLS streams from an origin, stores their segments, and serves them back to players as live streams and, once ingest finishes, as VOD.

## Channels and sources

**Channel**:
One stream the gateway ingests from a single source and serves under one id.
_Avoid_: Stream (for the configured unit), job

**Origin**:
The server a channel ingests from.
_Avoid_: Upstream, CDN

**Source**:
A channel's configured input: the origin URL of its manifest or playlist, and its protocol (DASH or HLS).
_Avoid_: Upstream, feed

## Playback

**Playback**:
One player's viewing of a channel. Its media position and play/pause intent belong to that player, independently of the channel's ingest lifecycle.
_Avoid_: Channel mode (for a player's viewing state)

**Presentation**:
What a channel offers to players: its tracks, the protocols it can be served as, and the DASH manifest or HLS playlists rendered from them for gateway playback or local playback.
_Avoid_: Output, manifest (for both protocols)

## Timeline

**Period**:
A continuous stretch of a channel's timeline. A new one starts when the origin restarts its numbering, including a restart that happened while the gateway was down; a restart of the gateway itself does not start one.
_Avoid_: Epoch, discontinuity

**Representation**:
One encoded track of a stream at one quality: a DASH Representation, an HLS variant, or an HLS audio or subtitle rendition.
_Avoid_: Rep (outside code), variant, rendition, track

**Segment number**:
A segment's position within its representation, taken from the HLS media sequence or the DASH `$Number$`.
_Avoid_: Sequence, index

**Timeline**:
One representation's periods in time order, each with the lowest and highest segment numbers it has ever published there and the origin discontinuities among them, kept after those segments leave the live window.

**Live index**:
The segment index a channel serves from while it is ingesting.

**VOD index**:
The segment index rebuilt from disk during the VOD transition, which a VOD channel serves from.

## Lifecycle

**VOD transition**:
The move of a channel from ingesting to VOD, which ends when the finished asset is published or fails.
_Avoid_: Finalization, rebuild

**VOD**:
A channel whose ingest is complete and which serves a fixed asset. A static source that is still being ingested is not yet VOD.
_Avoid_: Static channel, archive

**Channel mode**:
Where a channel is in its lifecycle: Live (ingesting a live source), Static ingesting (downloading a bounded source), Transitioning (in the VOD transition), VOD, or VOD failed (ingest has stopped and the VOD transition failed, but can be retried).
_Avoid_: State, status

## Encryption

**Key mode**:
How a channel handles encrypted content, fixed when it is created: Passthrough keeps ciphertext on disk (for AES-128 HLS, serving keys through the key proxy); Decrypt removes the encryption during ingest. For DASH, Decrypt needs content keys.

**Content key**:
A KID:key pair an operator supplies for a DASH channel in Decrypt mode, so ingest can decrypt the AdaptationSets that use that KID.
_Avoid_: License key, CEK

**Decrypt warning**:
A reason a DASH AdaptationSet in a Decrypt channel is not decrypted, or the channel is not served as HLS, reported in the channel status.

**Key proxy**:
The gateway endpoint that serves content keys to players in place of the origin.
_Avoid_: Key server, key relay
