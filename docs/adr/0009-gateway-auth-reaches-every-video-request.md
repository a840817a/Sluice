# The gateway auth header goes on every request for a channel's video

`upstream.auth_header` and `upstream.auth_value` set one header the gateway sends to origins. Since the first commit only the DASH and HLS watchers set it, so it reached MPD and playlist requests but not segments, inits, SegmentBase byte ranges or AES-128 keys. An origin that guards its segments with the same token as its manifest therefore served the manifest and refused every segment. Nothing recorded that as a choice, and `fetch.ApplyHeaders` claimed every origin request carried the same headers.

Every request for a channel's video now carries the gateway auth header after the channel's own `fetch_headers`, so the gateway value wins over a same-named channel one, as before. All of these requests go through one `fetch.Origin` per channel, shared by the watcher, the segment fetcher and the key cache. The header goes to whatever host a request names, including one a redirect leads to. We chose this over sending it only to the host of the channel's source URL: a channel's video is one task, and an operator who sets the header expects the whole task to authenticate with it. License requests are not video requests and keep their own headers (`internal/license`).

## Consequences

- When segments or keys live on another host, a CDN or a key server, that host receives the gateway token too. Neither `auth_header` nor a channel's `fetch_headers` can be limited to some hosts; a token that must not reach a host cannot be configured for a channel whose video that host serves.
- A request the origin did not need authenticated now carries the header anyway; origins ignore headers they do not check.
