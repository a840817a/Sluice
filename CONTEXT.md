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

## Timeline

**Period**:
A continuous stretch of a channel's timeline. A new one starts when the origin restarts its numbering.
_Avoid_: Epoch, discontinuity

**Representation**:
One encoded track of a stream at one quality: a DASH Representation, an HLS variant, or an HLS audio or subtitle rendition.
_Avoid_: Rep (outside code), variant, rendition, track

**Segment number**:
A segment's position within its representation, taken from the HLS media sequence or the DASH `$Number$`.
_Avoid_: Sequence, index

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
How a channel handles AES-128 HLS content: Passthrough keeps ciphertext on disk and serves keys through the key proxy; Decrypt removes the encryption during ingest.

**Key proxy**:
The gateway endpoint that serves content keys to players in place of the origin.
_Avoid_: Key server, key relay
