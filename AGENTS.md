# AGENTS.md

Sluice is a DASH/HLS gateway written in Go. Domain terms are defined in
`CONTEXT.md`; use them, and read `docs/adr/` before changing anything an ADR
covers. The repository directory is still named `mpd` (the project's old name);
`internal/mpd` keeps its name because it parses the DASH MPD format.

## Verifying a change

- `make verify` runs what CI runs: build, vet, gofmt, and `go test -race`. All
  four must pass; a file listed by `gofmt -l .` is a failure. The race suite
  takes about a minute, mostly `internal/httpapi`'s end-to-end tests.
- Measure coverage with `-coverpkg=./...`. Plain `go test -cover` credits each
  package only with its own tests, and reports well-tested packages such as
  `internal/channel` as uncovered.
- `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` checks
  vulnerabilities; `go run golang.org/x/tools/cmd/deadcode@latest ./cmd/...`
  finds unreachable code. No other linter is set up.

## Committing

- Do not add a `Co-Authored-By` trailer to commit messages. A
  `Claude-Session` trailer is fine.

## Writing tests

- After a new test passes, break the code it protects and confirm the test
  fails, then restore. Several tests in this repo passed against broken code.
- Tests of `manifest.Generate` or `hlsout` must put published segments in the
  index. An empty index passes against output no player can play.
- The parser numbers AdaptationSets by position, not by the MPD's `id`
  attribute: the first one is `"0"` in an index fixture, whatever the XML says.
- Under the default config the live index and the VOD index hold the same
  segments, so a bug that reads the wrong one cannot fail an end-to-end test
  unless it disables `keep_all_segments` and ingests more than 120 segments per
  representation. Prefer a table test inside package `channel`.
- CENC fixtures in `testdata/cenc` come from Bento4 (its README has the
  commands). Decrypt tests must compare against the `clear/` variant, because
  a wrong key decrypts without error.
- Longer procedures (refactor checks, admin UI checks, reproducing a Stop race)
  are in `docs/agents/testing.md`.

## Running the gateway by hand

- `go run ./cmd/gateway` runs a binary from the Go build cache, so
  `pkill -f cmd/gateway` misses it and a stale process keeps the port. Stop it
  by port (`lsof -nP -iTCP:<port> -sTCP:LISTEN -t`), and confirm a restarted
  gateway bound the port before trusting any result that depends on the restart.
- The admin UI is embedded with `go:embed`: rebuild and restart after editing it.
- With the built-in admin password the gateway only listens on loopback, so
  use `addr: "127.0.0.1:<port>"`.

## Agent skills

### Issue tracker

Issues live as GitHub issues in `a840817a/Sluice`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles, each label string equal to its name. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
