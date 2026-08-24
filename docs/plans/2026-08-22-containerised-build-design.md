# Containerised Build — Design

Date: 2026-08-22
Status: awaiting approval (revision 6)

Goal: build and run Sluice as a container, distributed as an image that hosts
pull, with a build pipeline that behaves the same on a developer Mac and on a
Linux host running either Docker or Podman.

> **Revision 7** records the Podman path being run for the first time. Four
> assumptions failed on contact: the image's process was never in group 0, so
> the volume-ownership scheme did nothing; Podman does not read the image's
> `HEALTHCHECK`; a health command that is not a JSON array is passed to
> `/bin/sh -c`, which a distroless image does not have; and two checks had been
> testing only the fixed configuration, which cannot show an instruction is
> necessary. All are fixed and re-run: 15 pass, 0 fail.
>
> **Revision 6** makes the bind mount the default data volume on every engine.
> Revision 5 had defaulted Podman to a named volume on the belief that a
> rootless bind mount required `:U`, which chowns the host directory and then
> needs `sudo` to inspect — defeating the visibility the bind mount was for.
> `keep-id` avoids that, so one default now behaves the same everywhere (§8).
>
> **Revision 5** keys container decisions to the **engine**, not the
> environment: a Podman host may be a development machine, and the earlier
> framing gave it production defaults and no conveniences (§7, §8, §9). It also
> resolves the pull-and-run question — the image ships a defaults-only config —
> and closes the `admin`/`admin` hole that decision would otherwise open (§5).
>
> **Revision 4** makes distribution the point of the exercise. Earlier revisions
> treated the registry as out of scope and judged containerisation on deployment
> ergonomics, where it barely beats `scp` of a static binary. The actual value is
> pull-based distribution: the target host needs no source and no toolchain, and
> versions become tags that can be rolled back. Registry publishing therefore
> moves in scope (§10), CI stops being a prepared-but-unexercised file and
> becomes the key path (§8), and the multi-arch manifest becomes required rather
> than a nicety (§4).
>
> **Revision 3** adds Podman as a real target. This is not a compatibility
> footnote — it changes the volume-ownership scheme (§4), the volume default
> (§8), and adds a Podman-native deployment artifact (§9). Nothing about Podman
> can be tested on the development machine, so §12 carries a separate checklist
> to run on the Podman host itself, and no untested claim is stated as fact.
>
> **Revision 2** replaces the URL strategy of revision 1. That version required
> `base_url` to hold the reachable address and had the Makefile detect the host
> IP to fill it. Investigation showed the code already supports relative URLs,
> which removes the problem instead of automating around it (§3).

## 1. Constraints established from the code

Verified, not assumed. These shape every decision below.

| Fact | Where | Consequence |
|---|---|---|
| Pure Go, no cgo, no `os/exec`, no ffmpeg | `CGO_ENABLED=0 go build` passes | Fully static binary; runtime image needs no libc |
| Front end is embedded, not built | `internal/web/embed.go:16` | No Node stage today; assets must exist under `internal/web/` *before* `go build` |
| Gateway fetches upstream over HTTPS | `internal/fetch` | Runtime image must carry a CA bundle |
| `internal/gwurl` is the single definition of URL shape | package doc, `gwurl.go` | Relative-URL support is a one-package concern (§3) |
| `base_url` has no default and no validation | `config.go:27`; absent from `applyDefaults` | Empty is already legal — relative output needs no new config plumbing |
| Only runtime state is `store.data_dir` | `config.go:92` | One volume; the runtime user must own it |
| Every `CreateTemp` writes inside the data dir | `store/writer.go:21`, `channel/store.go:114`, `hlskey/key.go:276` | No `/tmp` dependency → rootfs can be `read_only` |
| HLS AES-128 keys are written into the data dir | `hlskey/key.go:224,276` | That volume holds secrets; treat accordingly |
| Config is a single file via `-config` | `main.go:52` | No env support today — added in §5 |
| `/healthz` exists | `httpapi/routes.go:68` | Healthcheck target, but distroless has no curl (§6) |
| Self-signed cert takes exactly one IP | `main.go:52` `selfSignedCert(tlsCfg.IP)` | Unusable in containers as-is (§7) |

Re-verified 2026-08-22 against the working tree: all eleven `file:line`
references above still resolve to the lines described. The three base images in
§4 were pulled successfully, and `golang:1.26` is go1.26.7 — at or above the
`go 1.26.1` in `go.mod`, so no toolchain download happens during the build.

## 2. Principle: the image carries no environment

- **Build time** (`docker build`) — code and dependencies only. **Zero
  host-specific data.** The same image digest must run unmodified anywhere.
- **Run time** (`compose up`) — the host injects what is specific to it.

Every detection step in this design therefore lives in a *run* target
(`make up-tls`), never in a *build* target (`make image`). Any future addition
that bakes an address, hostname, IP, or credential into the image violates this.

## 3. Output URLs: relative by default

`internal/gwurl` already splits its builders into relative paths and absolute
URLs, and every absolute builder starts with `gwurl.Base(base)`, which is only
`strings.TrimRight(base, "/")`. With `base_url` empty:

| Output | `base_url` set | `base_url` empty |
|---|---|---|
| DASH `<BaseURL>` (`generator.go:132`) | `https://host:8443/` | `/` |
| DASH segment template | `v1/channels/…` (already relative) | unchanged |
| HLS media playlist / segment / init / key | `https://host:8443/v1/…` | `/v1/…` |

Both become root-relative references, which players resolve against the URL the
manifest itself was fetched from (RFC 3986 for DASH; the HLS spec permits
relative URIs directly). One configuration then serves
`http://localhost:8080`, `https://192.168.x.x:8443`, and any reverse proxy
simultaneously — with no detection, no injection, and no restart.

**So `base_url` becomes optional and defaults to empty.** It is not removed; it
stays as an override for the two cases relative URLs cannot cover:

1. A reverse proxy mounting the gateway under a sub-path (`/sluice/`), where
   root-relative `/v1/…` would escape the prefix.
2. The DRM license URL, if the test in §12 shows players do not resolve it
   relatively.

### The one unverified point

`gwurl.LicenseURL` with an empty base yields
`/v1/channels/{id}/license/playready`, which ends up in the MPD's
ContentProtection. **Whether Shaka resolves a relative license-server URL is
UNVERIFIED** and will be settled by the test in §12, not by reasoning. If it
does not resolve, the fallback is narrow: license URLs stay absolute, everything
else stays relative. Segment playback is unaffected either way.

## 4. Image build

Multi-stage, `distroless/static:nonroot` as the default runtime.

```
builder   golang:1.26  --platform=$BUILDPLATFORM   (native, cross-compiles)
runtime   gcr.io/distroless/static-debian12:nonroot
debug     alpine:3.20  (--target debug, has a shell)
```

The runtime base is ~2 MB, ships the CA bundle and tzdata, runs as uid 65532,
and has no shell or package manager. The `debug` target exists only so
`docker exec` is possible; it installs `ca-certificates` and creates the same
non-root user, so behaviour matches apart from the shell.

### Cross-compilation, not emulation

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" ...
```

Because the binary is cgo-free this needs no QEMU, so two architectures cost
barely more than one.

**Prerequisite, verified on the development Mac (Docker 29.7.2, storage driver
`overlay2`):** Buildx's default `docker` driver **cannot** produce multi-platform
images. The plain command fails with *"Multi-platform build is not supported for
the docker driver"*. A `docker-container` builder is required:

```bash
docker buildx create --name sluice-builder --driver docker-container --bootstrap
docker buildx build --builder sluice-builder   --platform linux/amd64,linux/arm64 -t sluice:latest .
```

Both halves were tested: the default builder fails, the `docker-container`
builder succeeds for both architectures. `make image-multiarch` therefore
creates the builder if absent rather than assuming one exists — this is the kind
of prerequisite that otherwise surfaces as a confusing CI-only failure. (Turning
on the containerd image store is the alternative fix; a named builder is chosen
because it is per-project and does not change a global Docker setting.)

That is what lets an arm64 Mac produce images for an amd64 Linux host. The image
is a Linux image either way; Docker Desktop's Linux VM is what runs it on macOS
or Windows. Native Windows containers are not supported and are not a goal.

`go.mod`/`go.sum` are copied and `go mod download` runs before the source, so
dependency layers survive source edits.

### Volume ownership — group 0, not a fixed uid

Distroless has no shell, so `mkdir`/`chown` cannot run in the runtime stage. The
directory is created in the builder and copied with ownership and mode:

```dockerfile
RUN mkdir -p /data && chmod 0775 /data
COPY --from=builder --chown=65532:0 /data /data
```

Note `:0` — the **group**, not `65532:65532`. Group 0 plus `g+rwX` is the
portable pattern, because rootless Podman maps uids into a subuid range where
the numbers do not correspond, while gid 0 survives everywhere — including
arbitrary-uid runtimes such as OpenShift.

This only works if the *process* is in group 0, which the base image does not
arrange: it declares `User: "65532"` with no group, giving gid 65532. The
runtime stage therefore sets `USER 65532:0` explicitly. Without that line the
ownership is decoration and only the owner bits apply — the failure mode is a
bind mount owned by anyone else, which is every rootful Podman host.

Without this a named volume mounts root-owned and the process cannot write
segments — surfacing as a permission error deep in the store, not at startup.

This fixes **named volumes**, where the engine seeds ownership from the image.
It does **not** fix a bind mount under rootless Podman, where the host
directory's own ownership wins — see §8 and §9.

### Front-end build hook (not built now)

No `package.json`, no bundler; a Node stage today would add ~200 MB for nothing.
The Dockerfile instead carries a commented `frontend` stage and a commented
`COPY --from=frontend /app/dist ./internal/web/static` in the builder, placed
before `go build` — `go:embed` only sees files present at compile time and only
under `internal/web/`. `.dockerignore` deliberately does **not** exclude
`internal/web/static`, but does exclude `node_modules/`.

## 5. Configuration

### Environment overrides

Precedence in `config.Load`: **YAML → environment → defaults**. Defaults run
last and only fill still-empty values, so an override is never clobbered.

**The image ships its own `/etc/sluice/config.yaml`**, holding only
container-appropriate values and comments — everything else comes from
`applyDefaults`. Mounting a file over that path replaces it, exactly as before.

This keeps `Load` strict (the file always exists inside the image) while making
`podman pull && podman run` work with no config supplied, which §10 needs. It
does not violate §2: defaults are environment-independent by definition, and
nothing host-specific is baked in.

Deliberately kept minimal rather than a copy of `config.yaml.example`. Two files
documenting the same defaults would drift, and the example is written for humans
choosing values, not for a container's baseline.

### Refusing an unsafe default

`applyDefaults` substitutes `"admin"` when `admin.password` is empty
(`config.go:132`), and the admin API — Basic Auth, able to create and delete
channels — is what that guards. Under the old model an operator wrote a config
file and saw the field. Under "pull and run", **anyone who pulls and runs gets
`admin`/`admin` on a service they may have just exposed.** The convenience
decision creates this, so it is fixed alongside it.

The gateway refuses to start when the admin password is still the built-in
default *and* the listener is not loopback-only, with a message naming
`SLUICE_ADMIN_PASSWORD`. Loopback keeps working unchanged, so local development
is unaffected. The considered alternative — generate a random password and log
it — was rejected because a logged secret is easy to miss in a headless deploy,
and a hard failure at start is cheaper than one discovered later.

Only two categories get overrides: **secrets**, and **deployment topology**.

| Variable | Field |
|---|---|
| `SLUICE_CONFIG` | config file path (currently `-config` flag only) |
| `SLUICE_SERVER_ADDR` | `server.addr` |
| `SLUICE_SERVER_BASE_URL` | `server.base_url` (override; empty = relative, §3) |
| `SLUICE_ADMIN_USERNAME` / `SLUICE_ADMIN_PASSWORD` | `admin.*` |
| `SLUICE_STORE_DATA_DIR` | `store.data_dir` |
| `SLUICE_UPSTREAM_AUTH_HEADER` / `SLUICE_UPSTREAM_AUTH_VALUE` | `upstream.*` |
| `SLUICE_TLS_SELF_SIGNED` / `SLUICE_TLS_IP` | `server.tls.*` |
| `SLUICE_TLS_CERT_FILE` / `SLUICE_TLS_KEY_FILE` | `server.tls.*` |

Tuning knobs (`worker.*`, `window.*`, `upstream.poll_interval`,
`store.cleanup`) stay YAML-only. Rule for future additions: **a field earns an
env override only if it is a secret or changes between deployments of the same
build.**

The two secrets also accept a `_FILE` suffix
(`SLUICE_ADMIN_PASSWORD_FILE`, `SLUICE_UPSTREAM_AUTH_VALUE_FILE`) reading the
value from a path, so Docker and Kubernetes secrets — which arrive as mounted
files — work without a wrapper.

### Delivering configuration *files*

Design rule: **every file input is addressable by a path, and that path is
settable by an environment variable.** Given that, all delivery mechanisms are
interchangeable with no code change. Convention: everything mounts under
`/etc/sluice/`.

| Mechanism | Location | Use for |
|---|---|---|
| Bind mount | `./config.yaml:/etc/sluice/config.yaml:ro` | Local development, live editing |
| Compose `configs:` | compose-managed file | Non-secret config, no host path |
| Compose / Swarm `secrets:` | `/run/secrets/<name>` | Private keys, tokens (tmpfs-backed) |
| K8s ConfigMap / Secret volume | any mount point | Production clusters; identical shape |

Baking files into the image is acceptable only for values identical in every
environment — the §2 principle again.

**Permissions.** The process runs as uid 65532. Under Docker a bind-mounted
`0600` file owned by the host user is unreadable inside the container, and the
symptom is a bare "permission denied" that does not mention uid. Under rootless
Podman with `keep-id` (§8) the same file *is* readable, because the invoking
user maps onto 65532 — so this trap is Docker-specific, which is exactly why it
is easy to miss when moving between engines. Use `0644` for non-secret files;
use compose `secrets:` with `uid: "65532"` for secret ones.

**Private or corporate CA** (upstream origin with a non-public CA) needs no code
change — Go honours `SSL_CERT_FILE` / `SSL_CERT_DIR` on Linux:

```yaml
volumes:   [./corp-ca.pem:/etc/sluice/ca.pem:ro]
environment: { SSL_CERT_FILE: /etc/sluice/ca.pem }
```

Note this **replaces** rather than augments the system bundle, so the file must
concatenate the corporate CA with the public roots or ordinary HTTPS upstreams
will fail.

**Read-only rootfs.** Since no code writes to `/tmp` (§1), the container runs
with `read_only: true` and only the data volume writable.

## 6. Healthcheck

Distroless ships no curl, and adding one defeats the base image. `cmd/gateway`
gains a `-healthcheck` flag that GETs `http://127.0.0.1{addr}/healthz` and exits
0 or 1, so the container health checks itself with its own binary:

```dockerfile
HEALTHCHECK CMD ["/sluice", "-healthcheck"]
```

## 7. Inbound TLS

With §3 in place, TLS is the *only* thing still needing the host's address, and
only when serving HTTPS. Plain HTTP on localhost needs no detection at all.

### Self-signed SAN fix

`selfSignedCert(ip)` currently writes at most one IP into the SAN. The fix:

- Always include `localhost`, `127.0.0.1`, `::1`.
- `server.tls.ip` accepts a **list**, of IPs and DNS names, and is **additive**
  rather than the only source. Existing single-value configs keep working.
- Interface enumeration is kept but **explicitly demoted to a fallback**: inside
  a container it sees only the bridge address (`172.x`), never the host's LAN
  address, so it must not be relied on. Documented as such so nobody assumes the
  container case is covered.

What this does **not** fix: the browser still warns, because the certificate is
self-signed. That is inherent, and is why the Caddy option is deferred, not
dropped (§11).

### Host IP detection is a convenience, not a mechanism

A Podman machine is **not necessarily production** — it may equally be a Linux
development box that wants exactly this convenience. So the mechanism must not
be tied to Docker or to compose.

`scripts/host-ip.sh` resolves the address on the **host**, where the answer is
correct, and writes `SLUICE_TLS_IP` into `.env`. **`.env` is the shared
contract**: compose consumes it via `env_file:`, a Quadlet unit via
`EnvironmentFile=`. Neither the script nor the variable knows which engine or
launcher is in use, so the same `make up-tls` convenience is available on a
Podman host. Three rules keep it portable across machines that clone this repo:

1. **Derive the interface from the default route**, never hardcode `en0`. macOS
   and Linux each get one branch. (On the development Mac: default route → `en0`
   → `192.168.188.134`, a single-interface host — deliberately not treated as
   representative.)
2. **Overridable**: `HOST_IP=203.0.113.5 make up-tls`, or set it in `.env`. This
   is the path for NAT'd servers, VPN interfaces (`utun*`, Tailscale), and
   multi-homed hosts, where detection would pick the wrong address.
3. **Fail loudly**: empty detection aborts with the exact override command.
   Guessing wrong produces a confusing playback failure, not an obvious one.

The detected value is never committed — every machine differs, and `.env` is
gitignored.

Startup logs the effective SAN list and `base_url` so a mismatch is visible
immediately rather than as a downstream player error.

Production TLS is a separate matter: a real deployment terminates TLS with
`cert_file`/`key_file` or a reverse proxy, and needs none of this. The
self-signed path exists for testing, on either engine.

**Why TLS is needed at all:** DRM (EME) requires a secure context.
`http://localhost` qualifies, so local testing over plain HTTP plays fine.
`http://<LAN-IP>` does not — so testing DRM from a phone or another machine
needs HTTPS. That is the scenario this section serves, and the only one.

## 8. Compose, Makefile, CI

`compose.yaml`, one service, `docker compose up` works with no arguments:

- Ports `8080` (HTTP) and `8443` (TLS, only used when enabled).
- `./config.yaml` bind-mounted read-only at `/etc/sluice/config.yaml`.
- `read_only: true` rootfs; data volume the sole writable path.
- `restart: unless-stopped` plus the §6 healthcheck.
- `.env.example` documents the variables; the real `.env` is gitignored.

**Data volume: bind mount by default on every engine.**

`store.cleanup` defaults to `"disabled"` (`config.CleanupDisabled`) — segments
are never deleted. On bare metal that consumes a visible disk. In a container
writing to a *named* volume it grows unseen inside the engine's storage until
the host fills up and unrelated services start failing. A live channel running
for days makes this likely, not theoretical. The volume also holds AES-128 keys
(§1), so it is not merely bulk cache.

That argues for a bind mount, and it is the default on **both** engines. The
earlier revision defaulted Podman to a named volume; that was wrong for two
reasons. It assumed the only way to make a rootless bind mount work was `:U`,
which chowns the host directory to a mapped subuid and therefore requires
`sudo` to inspect or delete — destroying the visibility that motivated the bind
mount. And an unseen, unbounded data directory is *more* dangerous on a server,
which is what a Podman host often is.

| Engine / host | Requirement for the bind mount |
|---|---|
| Docker Desktop (macOS) | none — the sharing layer maps ownership to the host user. **Verified** |
| Docker (Linux) | `SLUICE_UID=$(id -u)`, which `make up` passes. **UNVERIFIED** — no Linux Docker host |
| Podman rootful | the directory must be group 0 with `g+rwX` (`chmod 0775` on a root-owned directory), or owned by 65532. **Verified 2026-08-22: fails without it** |
| Podman rootless | `UserNS=keep-id:uid=65532,gid=65532` — host ownership stays yours |
| SELinux (Fedora/RHEL) | `:Z` on the mount. **Verified 2026-08-22** |

**What "uids match numerically" got wrong, three times.** Revisions 3 to 6 each
asserted some variant of it, and each was wrong for the same reason: what
decides writability is *who owns the host directory*, never whether a number
appears on both sides. Docker on macOS hid it by ignoring ownership entirely,
which is the machine this was designed on.

The third instance was in the image itself. `distroless/static:nonroot`
declares `User: "65532"` with no group, so the primary gid comes from its
`/etc/passwd` and is 65532 — not 0. The `--chown=65532:0 --chmod=0775` on
`/data` therefore bought nothing beyond the owner bits, and the local test that
"proved" the gid-0 scheme passed only because it was run with an explicit
`--user 12345:0`, exercising a mode the image never used. The runtime stage now
sets `USER 65532:0` so the group membership is real.

**Docker on Linux was a gap in earlier revisions.** They claimed uids "match
numerically" for Docker generally, which is only true because Docker Desktop on
macOS ignores the numbers entirely — the machine this was designed on. On Linux
`./data` belongs to the invoking user, the container runs as 65532, and writes
fail. compose therefore takes `user: "${SLUICE_UID:-65532}:${SLUICE_GID:-0}"`
and the Makefile passes the host uid unconditionally; it is a no-op on macOS and
the fix on Linux. Verified working on macOS in both modes; the Linux case cannot
be tested here and is listed as unverified rather than assumed.

`keep-id` is the important one: it maps the invoking user to the container's
uid, so `./data` stays owned by you on the host *and* is writable inside. Two
documented lines, in exchange for one default that behaves the same everywhere.

A **named volume remains available** via `DATA_MOUNT` for anyone who wants the
engine to own the lifecycle — and the gid-0 scheme in §4 is what makes that work
without a branch. It is the opt-in, not the default.

The Makefile exposes `DATA_MOUNT` so either can be selected without editing
compose. README documents what `cleanup: disabled` costs **in both cases**,
including how to check consumption of a named volume — the invisible one is the
case that needs the instruction.

Makefile — so local and CI verification are the same commands, not two drifting
copies. macOS ships **GNU Make 3.81**, so no 4.x-only syntax (`$(file …)`, etc.).
Two variables keep it engine-agnostic: `ENGINE ?= docker` and `DATA_MOUNT`.

```
make build test verify           # go build / test -race / the four-check pass
make image image-multiarch       # branches on ENGINE (§9) — buildx vs manifest
make up down logs up-tls         # compose wrappers
make ENGINE=podman image         # same targets, other engine
```

`image-multiarch` must branch rather than merely substitute a binary name:
Docker uses `buildx build --platform a,b`, Podman uses
`build --platform a,b --manifest`. Those are different command shapes, so a
single templated line would silently do the wrong thing on one of them.

`make verify` runs the existing four-check pass: `go build ./...`,
`go vet ./...`, `gofmt -l .` (any output is a failure), `go test -race ./...`.

GitHub Actions runs `make verify`, then builds and **pushes** the multi-arch
image to GHCR (§10). With distribution as the goal this is no longer a
convenience file: it is how images come to exist.

**Published images come from CI, not from a laptop.** A locally built image
carries whatever was in that working tree, which is exactly the ambiguity tags
are supposed to remove. `make image` stays for development and as a break-glass
path, but anything a host pulls should be traceable to a commit.

`origin` now points at `github.com/a840817a/Sluice`, but nothing has been
pushed yet and the branch name is undecided — see step 0 of §13.

## 9. Container engine portability: Podman

Hosts may run Podman rather than Docker — a server, a test box, or a Linux
development machine equally. The differences that matter are not cosmetic — one
of them changes §4 and §8 above — so they are designed around rather than
papered over with a compatibility note.

**Verified on a real host, rootful and rootless, on 2026-08-24.** Four of the
assumptions below turned out to be wrong when run; see the results at the end of
this section.

### The difference that matters: rootless uid mapping

| | Docker (rootful) | Podman (rootless) |
|---|---|---|
| Container uid 65532 maps to host | uid 65532 | a subuid (e.g. 165531) |
| A host file owned by the invoking user | appears as that uid | appears as **uid 0** |
| Bind-mounted `./data` writable by 65532 | yes, if ownership matches | **no** — unless `keep-id` (below) |

Two mitigations exist for bind mounts. `./data:/data:U` makes Podman chown the
host directory to the mapped subuid — which then needs `sudo` to read, so it is
**not** used here. `--userns=keep-id:uid=65532,gid=65532` maps the invoking user
onto the container's uid instead, leaving host ownership intact; in a Quadlet
unit that is a single `UserNS=` line. §8 therefore keeps the bind mount as the
default on every engine and uses `keep-id` for rootless, rather than switching
the default to a named volume.

Rootful Podman needs neither — and rejects `keep-id`, which is rootless-only. So
the Quadlet unit carries the line with a comment saying to drop it when running
rootful, rather than silently failing for the other half of Podman users.

### Podman is an engine choice, not an environment

Nothing below assumes a Podman host is production. It may be a Linux
development machine, a test box, or a server; the engine differences apply the
same either way. Where earlier revisions wrote "development" and "production",
read "Docker workflow" and "Podman workflow" — §7's TLS convenience and §8's
volume default are both keyed to the engine for this reason.

### Deployment artifact: Quadlet, not podman-compose

`podman-compose` is a third-party reimplementation with uneven coverage of the
compose spec, and the features this design leans on — `secrets:`, `configs:`,
`read_only:`, healthchecks — are exactly the areas where coverage is uneven.
Depending on it would make the production path the least-tested one.

Instead the Podman host gets a **Quadlet** unit (`sluice.container`, a systemd
unit generated by Podman), which is the engine-native path and gives `Restart=`,
ordering, and journald logging for free. `compose.yaml` remains the Docker-side
artifact. Both consume the same image and the same `.env` (§7), so there is one
configuration contract, not two — and a Podman *development* box gets the same
conveniences as a Docker one, rather than being treated as production-only.

The `_FILE` secret convention (§5) pays off here: Quadlet's `Secret=` and
`podman secret` deliver secrets as files, needing no code change.

### Other differences, all documented rather than assumed

- **Multi-arch**: Podman has no `buildx`. Different command shape — see the
  Makefile note in §8.
- **SELinux**: on Fedora/RHEL, bind mounts need `:z` / `:Z` or the container
  cannot read them. Affects Docker too, but Podman users hit it far more often.
- **Low ports**: rootless Podman cannot bind <1024 by default. 8080/8443 are
  fine, but `config.yaml.example` suggests `addr: ":443"` for TLS — that path
  needs a proxy, `net.ipv4.ip_unprivileged_port_start`, or a port mapping.
  README must say so where it suggests `:443`.

### Verified on real hosts, 2026-08-24

Podman 5.8.2, SELinux enforcing, tested **rootful and rootless**:
15 pass, 0 fail. Everything this section previously listed as an assumption is
now settled.

| Was assumed | Result |
|---|---|
| Buildah honours `FROM --platform=$BUILDPLATFORM` and `TARGETOS`/`TARGETARCH` | Works |
| gid-0 ownership makes the named volume writable | Works |
| `keep-id` gives a writable bind mount **and leaves host ownership alone** | Works — directory still owned by uid 1001 afterwards |
| Podman honours the image `HEALTHCHECK` | **False.** Not read; the unit declares `HealthCmd=` in JSON-array form |
| `ReadOnly=true` needs no extra tmpfs | Correct |
| `:Z` is needed on SELinux | Correct, and *necessary* — the mount is denied without it |
| A `_FILE` secret needs no wrapper | Correct |
| Rootless cannot bind low ports | Correct — `pasta` refuses 443 with EPERM |

The rootless and rootful bind mounts differ in the way the design predicted: at
`root:root 0755` rootful is correctly denied and needs `chmod 0775`, while
rootless succeeds because `keep-id` maps the invoking user onto 65532.

### Still not verified

- **Docker on Linux** (`SLUICE_UID`). No such host was available. The reasoning
  is identical to rootful Podman, which is now confirmed — but reasoning is not
  a test, which is the lesson this section exists to record.
- Whether journald captures the unit's output. The test user could not read the
  journal at all, which says nothing either way.

Everything else in this section has been run. The Quadlet unit was started
rootless under systemd on 2026-08-24: healthy, serving, and recovered within
five seconds of `podman kill -s KILL`, confirming `Restart=always`. That start
is also the only test of `HealthCmd=` as Quadlet parses it, rather than as a
`podman run` argument.

### Third run, 2026-08-24

13 pass. `USER 65532:0` confirmed: the bind mount is correctly **not** writable
at `root:root 0755` and writable after `chmod 0775`, so the instruction is now
known to be both necessary and sufficient. `:Z` likewise passes in both
directions.

The remaining failure was the fix itself. An explicit `--health-cmd` failed too,
for a reason the design had already accounted for one level up: Podman documents
that a health command which is not a JSON array is "interpreted as an argument to
`/bin/sh -c`", and this image has no shell — the same fact that made the gateway
probe itself rather than call curl. `HealthCmd=` and `--health-cmd` therefore
have to use the JSON array form.

Worth stating plainly, because it is the pattern rather than the incident: every
Podman finding so far has been an assumption that looked verified. The gid-0
scheme was tested with an explicitly passed `--user 12345:0`, exercising a mode
the image never ran in. The bind mount and `:Z` were tested only in the fixed
configuration, which cannot show an instruction is necessary. The healthcheck
fix was written from the same shell-less premise it then ignored. None of these
survived contact with a real host, and none would have been caught by more
careful reading.

### Second run, 2026-08-22, with `--build`

10 pass, 2 fail. **Buildah supports `FROM --platform=$BUILDPLATFORM` and the
automatic `TARGETOS`/`TARGETARCH` args** — UNVERIFIED #1 is settled, and it was
the largest open question about the build. Both failures were the two above; the
bind mount failed against a `root:root 0755` directory, which is the documented
failure the `chmod 0775` step exists for, on an image built before the
`USER 65532:0` fix was published.

Two checks were only proving themselves. The bind mount and `:Z` were each
tested in the fixed configuration alone, which shows an instruction is
sufficient but never that it is *necessary* — and an unnecessary instruction in
a deployment document is how superstition propagates. Both now run twice, with
and without, and fail if the unfixed case unexpectedly succeeds.

Still unverified: everything rootless, including `keep-id` and the low-port
failure mode. This host was rootful.

## 10. Distribution: GHCR

This is the reason the project is being containerised. A host running Sluice
needs a registry credential, a tag, and an admin password — no source, no Go
toolchain, no build step, no config file to prepare, and a rollback that is one
tag away.

The password is not an oversight in that list. The image listens on `:8080`,
which is not loopback, so §5's guard requires `SLUICE_ADMIN_PASSWORD` before it
will start. Binding the image to loopback instead would satisfy "run with
nothing" literally while making the published port unreachable, which is worse
than one required variable.

**Resolved:** the earlier "config file is mandatory for the operator to supply"
rule is dropped in favour of pull-and-run convenience. The image ships its own
defaults-only config (§5); env vars override it; mounting a file replaces it.
Convenience does not extend to the admin password — §5 makes the gateway refuse
to start on the built-in default when it is reachable off-loopback, because
"pull and run" would otherwise mean "pull and run with `admin`/`admin`".

### Registry and naming

`ghcr.io/a840817a/sluice`. GHCR is chosen because it is free for private
packages, authenticates from Actions with the built-in `GITHUB_TOKEN` (given
`packages: write`), and has complete multi-arch manifest support.

**Casing trap:** the repository is `a840817a/Sluice`, with a capital S, but OCI
reference names must be lowercase. The idiomatic
`${{ github.repository }}` yields `a840817a/Sluice` and the push **fails**. The
workflow lowercases the name explicitly rather than relying on the repository
happening to be lowercase — which here it is not.

A GHCR package inherits the repository's visibility. A private repo yields a
private package, so **the target host must authenticate to pull** — see below.

### Tags

| Tag | When | Purpose |
|---|---|---|
| `sha-<short>` | every push to `main` | Immutable; every build is addressable |
| `<semver>` e.g. `1.4.2` | on git tag `v*` | What a host pins to |
| `1.4`, `1` | on git tag `v*` | Moving minor/major for patch pickup |
| `latest` | on git tag `v*` | Newest **release** — never a `main` build |

`latest` deliberately never tracks `main`: a host that pulls `latest` should get
something released, not the last commit that happened to build.

### Multi-arch is now required, not a nicety

One tag has to serve both an amd64 server and an arm64 machine, because whoever
pulls should not have to know or care. That makes the cross-compilation scheme
in §4 load-bearing: CI pushes a manifest list covering `linux/amd64` and
`linux/arm64`, and because the binary is cgo-free this costs almost nothing.

### Authenticating the target host

`podman login ghcr.io` with a token holding `read:packages`. The credential
lands in `${XDG_RUNTIME_DIR}/containers/auth.json`, which for a Quadlet unit
under systemd is supplied as a file — the same config-file delivery problem as
§5, solved the same way, with `AuthFile=` on the Quadlet unit.

Use a **dedicated read-only token**, not a personal one with write scope: this
credential sits on a deployment host, and a pull-only token cannot publish.

### What this makes possible

- `podman pull ghcr.io/a840817a/sluice:1.4.2` — the entire "get the software
  onto the machine" step.
- Rollback is `1.4.1`, not a rebuild.
- Several hosts provably run the same bytes.
- The Mac stops being a single point of failure in the release path.

## 11. Deliberately out of scope

- **Caddy / automatic Let's Encrypt.** Deferred by decision; §7 covers the
  testing pain and a real deployment can front the HTTP port with any proxy.
  Revisit when a domain name exists.
- **A config schema beyond the two categories in §5.** Tuning knobs stay
  YAML-only; env coverage is not expanded to every field.
- **Front-end build stage.** Hook only (§4).
- **Image signing / SBOM / provenance attestation.** Worth revisiting once
  publishing works; not a prerequisite for it.

## 12. Verification plan

Automated, added alongside the code:

- `internal/config`: table-driven precedence tests — YAML only, env beats YAML,
  `_FILE` beats unset, malformed bool rejected.
- `cmd/gateway`: parse the generated self-signed certificate; assert the SAN set
  contains loopback, every configured extra entry, and DNS names.
- `internal/manifest` / `internal/hlsout`: assert that an empty base produces
  root-relative URIs in both the MPD and the m3u8.

Manual — Docker path, requiring Docker Desktop running and a playable DRM
channel:

- `docker build` for both targets; `buildx` for two architectures **via a
  `docker-container` builder** — the default driver cannot, as §4 records.
- Non-root write to the data volume actually succeeds.
- `read_only: true` rootfs starts and serves.
- Playback via `http://localhost:8080`.
- Playback via `https://<LAN-IP>:8443` from another device. **`make up-tls`
  verified 2026-08-22**: `/healthz` answered over TLS at the LAN address, and
  the certificate's SAN held `localhost, 127.0.0.1, ::1, 192.168.188.134,
  172.19.0.2`. The container's own enumeration contributed only `172.19.0.2` —
  without the host-side injection the LAN address would have been absent, which
  is the failure §7 exists to prevent.
- **The `base_url`-empty license URL question (§3)** — the one item that decides
  whether relative output is complete or needs a narrow exception.

Pull-and-run (§5, §10):

- A container started with **no config mounted and only `SLUICE_ADMIN_PASSWORD`
  set** serves `/healthz`. **Verified 2026-08-22.**
- The same container with **nothing** set refuses to start and names
  `SLUICE_ADMIN_PASSWORD` in the message — the image listens on `:8080`, so the
  §5 guard applies. **Verified 2026-08-22.**
- An empty config file loads as "override nothing" rather than failing to parse.
- Mounting a config file over `/etc/sluice/config.yaml` still wins.

Distribution (§10), verifiable once step 0 of §13 is done:

- CI pushes a manifest list; `docker manifest inspect` shows both
  `linux/amd64` and `linux/arm64` under one tag.
- Pulling `sha-<short>` on the Mac yields a runnable arm64 image.
- A read-only token can pull and **cannot** push.
- Tagging `v0.1.0` produces `0.1.0`, `0.1`, `0`, and moves `latest`; a plain
  `main` push does **not** move `latest`.

Manual — Podman path. **Run and passing, 2026-08-24** (Podman 5.8.2, SELinux
enforcing, rootful and rootless): `scripts/verify-podman.sh --build` reports
15/15, and the Quadlet unit was additionally started under systemd — healthy,
serving, and recovered within five seconds of `podman kill -s KILL`. The script
is the checklist; run it again after touching the Dockerfile, the unit, or
anything about volumes.

## 13. Implementation order

**All steps complete as of 2026-08-24.** Kept as the record of what was done in
what order, and why step 2 was pulled forward.

Each step ended with the four-check pass and its own commit.

0. **GitHub remote.** `origin` is set to `https://github.com/a840817a/Sluice.git`
   but the remote is **empty** — nothing has been pushed. Two things remain, both
   yours: decide whether the branch is `master` (current local name) or `main`
   (the convention this repo's tooling assumes, and what §10's tag rules are
   written against), then push. Pushing is outward-facing, so it waits for you.
1. `.dockerignore`, Dockerfile (both targets); verify a real `docker build`.
2. Relative-URL tests + make `base_url` empty the documented default.
3. `internal/config` env overrides, `_FILE` support, `SLUICE_CONFIG`, and the
   unsafe-default refusal (§5) + tests.
4. Self-signed SAN fix (list, additive, demoted enumeration) + tests.
5. `-healthcheck` flag.
6. `compose.yaml`, `.env.example`, `scripts/host-ip.sh` (writing `.env`, shared
   by compose and Quadlet per §7).
7. Makefile.
8. CI workflow: `make verify`, then multi-arch build and push to GHCR (§10),
   including the tagging rules.
9. Cut `v0.1.0` and verify the published tag set and manifest list end to end.
10. Quadlet unit `sluice.container` for the Podman host (§9), with `AuthFile=`
    for the GHCR credential.
11. README: build/run rewritten around **pulling an image** as the primary path,
    with build-from-source secondary; plus the `cleanup` warning (both mount
    styles), the config-file delivery table, the rootless low-port caveat, and
    the Podman section marked untested.

Steps 1–9 and 11 are verifiable here **once step 0 is done**. Step 10 (Quadlet)
is written but **cannot be verified on this machine**; it ships marked untested,
with the §12 Podman checklist as its acceptance criteria for whoever runs it on
the target host.

Step 2 comes early because its outcome may narrow step 4 and 6 — if relative
URLs fully work, host detection stays a convenience; if the license URL needs an
absolute base, that changes what `make up-tls` must inject.

## 14. What is still open

Two items, neither blocking, both stated here rather than left to be discovered.

**The DRM license URL under an empty `base_url` (§3).** Never tested. Making
output relative is the change everything else was built on, and this is the one
part of it that cannot be settled by reading a spec: `gwurl.LicenseURL` with an
empty base yields `/v1/channels/{id}/license/playready`, and whether a player
resolves a relative license-server URL from the manifest is player behaviour.
Segment playback is unaffected either way — the fallback is narrow, keeping
license URLs absolute while everything else stays relative — but until a DRM
channel is played from a device that is not the gateway host, the default ships
unproven for DRM content. Non-DRM playback is verified.

**Docker on Linux (`SLUICE_UID`).** No such host was available. The reasoning is
identical to rootful Podman, which is confirmed; reasoning is not a test, which
is the whole lesson of §9.
