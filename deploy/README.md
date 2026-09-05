# Deploying Sluice

Three launchers, one image and one `.env` between them:

| File | Engine | Notes |
|---|---|---|
| `../compose.yaml` | Docker | Verified working |
| `sluice.container` | Podman + systemd (Quadlet) | Container behaviour verified; the unit itself is parsed, not yet started |
| `bootstrap.sh` | Podman + systemd (Quadlet) | Same install on a host with no checkout; carries the unit inline |
| `config.container.yaml` | both | Baked into the image; not deployed by hand |

## Podman: verified 2026-08-24

Podman 5.8.2, SELinux enforcing, **rootful and rootless**: 15 pass, 0 fail.

```bash
./scripts/verify-podman.sh --build
```

Re-run it after changing the Dockerfile, the unit, or anything about volumes —
four of the assumptions it now guards were wrong the first time it ran.

- [x] `podman build`, and multi-arch with `--platform ... --manifest`. Buildah
      honours `FROM --platform=$BUILDPLATFORM` and `TARGETOS`/`TARGETARCH`.
- [x] **Rootless with `keep-id`**: writable, and `~/sluice/data` is still owned
      by you afterwards. That second half is why `keep-id` is used and `:U` is
      not.
- [x] **Rootful**: needs `chmod 0775` on the data directory (see Setup). Denied
      without it, which is checked both ways so the instruction is known to be
      necessary rather than copied.
- [x] Named volume via `DATA_MOUNT` — what the image's `65532:0` with `g+rwX`
      exists for.
- [x] SELinux `:Z` — the mount is denied without it and works with it.
- [x] `podman healthcheck run` — **the image's `HEALTHCHECK` is not read by
      Podman**, so the unit declares `HealthCmd=` itself, in JSON-array form: a
      plain string is passed to `/bin/sh -c`, and this image has no shell. A
      plain `podman run` likewise has no healthcheck unless you pass
      `--health-cmd`.
- [x] `ReadOnly=true` starts and serves, with no extra tmpfs.
- [x] A password from `podman secret` is read through `SLUICE_ADMIN_PASSWORD_FILE`.
- [x] Rootless refuses ports below 1024 — `pasta` returns EPERM for 443.
- [x] **The unit runs.** Started rootless under systemd: `active (running)`,
      `Up (healthy)` — which is also the only test of `HealthCmd=` in Quadlet
      rather than on a `podman run` command line — `healthz` 200, and after
      `podman kill -s KILL` it was back and healthy within five seconds, so
      `Restart=always` works.
- [ ] Reading its logs with `journalctl --user -u sluice` returned "insufficient
      permissions" on the test host: that user was in none of `adm`,
      `systemd-journal` or `wheel`, and the host has no persistent journal. That
      is journal configuration, not a defect in the unit — confirm from root
      with `journalctl _SYSTEMD_USER_UNIT=sluice.service` if it matters to you.

## Setup

```bash
./install-quadlet.sh
```

Does everything below and starts the service. It refuses to continue until
`SLUICE_ADMIN_PASSWORD` is set, and it adapts to the host: `chmod 0775` on the
data directory for rootful, the `UserNS=keep-id` line dropped there because
rootful Podman rejects it, and `:Z` removed where SELinux is off.

**Not yet run.** Written from the steps that were performed by hand on a
verified host, but the script itself has not been executed — see the note at the
top of this file about what that is worth.

### On a host with no checkout

```bash
curl -fsSL https://raw.githubusercontent.com/a840817a/Sluice/main/deploy/bootstrap.sh | sudo bash
```

Same result as `install-quadlet.sh`, but it carries the unit inline instead of
reading `sluice.container`, so nothing has to be cloned. **If you change
`sluice.container`, change the heredoc in `bootstrap.sh` too** — nothing enforces
that, so it is worth diffing the directive names when either one moves.

Defaults to `:latest`; pass a tag (`bash -s -- 0.1.1`) to pin. Re-running it is
the upgrade path for a moving tag: Podman's default pull policy is `missing`, so
`systemctl restart` alone keeps starting the image already on disk.

Before installing anything it runs the gateway's own writability check inside
the real image, against the real mount, user namespace and SELinux label:

```bash
podman run --rm --read-only -v "$DATA_DIR:/data:Z" \
    --userns=keep-id:uid=65532,gid=65532 \
    $IMAGE -config /etc/sluice/config.yaml -check-store
```

(`--userns` on rootless only, and `:Z` on SELinux only — the script adds each
where the host needs it, exactly as it does for the unit.)

If `/data` is not writable it prints the directory's uid/gid/mode next to the
uid/gid the process runs as, and stops without installing the unit. That timing
is the point: the unit is `Restart=always`, so installing first would leave a
service crash-looping and enabled at boot over a fault that was already
knowable.

It is the same `store.CheckWritable` the gateway calls at startup, so there is
one definition of "writable" rather than a shell reimplementation that drifts
from it — and the check covers every cause at once: a mode or owner the process
does not match, an image that declares `USER` with no group (before 0.1.1, the
primary gid came from the image's own `/etc/passwd` and was 65532, not 0), a
missing `:Z`, a read-only mount.

`-check-store` arrived after 0.1.1. Against an older tag the script says so and
continues rather than refusing — those images work when the volume is right,
they just cannot say so in advance — but 0.1.0 is the one that cannot write a
group-0 directory at all, so a warning there is worth acting on.

`-check-store` runs ahead of the admin-password policy, so it works on a host
that is not configured yet. `cmd/gateway/checkstore_test.go` pins that ordering,
because a password requirement there would fail identically whether the mount
was good or bad.

#### TLS, and why a DRM channel needs it

```bash
SLUICE_TLS_SELF_SIGNED=true SLUICE_TLS_IP=<this host's IP> bash bootstrap.sh
```

Browsers expose EME only in a secure context. Served over plain HTTP to
anything but localhost, `navigator.requestMediaKeySystemAccess` does not exist
at all and a DRM channel fails in the player with shaka error 6020,
`MISSING_EME_SUPPORT` — not 6001, which is what an unsupported key system looks
like. Clear content plays fine, which is what makes this confusing: the gateway,
the manifest and the keys are all working.

`SLUICE_TLS_IP` matters more here than on a host install. The self-signed
certificate covers every address the gateway can see, but it sees a container
network namespace — never the host's LAN address. Without this the certificate
will not carry the address people type, and a name mismatch is the one TLS
warning a browser will not let you click through. The script fills it in from
the default route when TLS is on and the value is missing.

The settings live in the same `.env` as the password. A re-run that says nothing
about TLS leaves them alone, and re-running with only `SLUICE_TLS_IP` changes
the address without turning TLS off.

There is one listener, so TLS is served on whatever port the container binds —
publishing stays `SLUICE_HTTP_PORT`. Publishing 443 while serving plain HTTP is
called out at the end of a run, because `https://<host>/` then does not connect
at all.

### By hand

```bash
mkdir -p ~/sluice/data
cp ../.env.example ~/sluice/.env      # set SLUICE_ADMIN_PASSWORD; it starts empty
```

The container runs as uid 65532, group 0, so the data directory has to be
writable by that:

```bash
# Rootful (the directory is root-owned, so its group is already 0):
chmod 0775 ~/sluice/data

# Rootless: nothing to do — UserNS=keep-id in the unit maps you onto 65532,
# and the directory stays owned by you.
```

Skipping this on a rootful host used to produce a permission error from deep
inside the segment store, minutes or days after a start that reported healthy —
the first failure found when this was run on a real Podman host. The gateway now
checks the directory at startup and refuses to run, so the mistake is loud and
immediate whichever launcher you use.

The gateway refuses to start with its built-in password on any address that is
not loopback, so this is not optional.

## Upgrading

Edit the `Image=` tag, then:

```bash
systemctl --user daemon-reload
systemctl --user restart sluice
```

Or, if the unit was installed by `bootstrap.sh` at `:latest`, re-run it — the
restart on its own will not pull a newer image.

Rolling back is the same edit with the previous tag. Every push to `main` also
publishes an immutable `sha-<short>` tag if you need to pin something that was
never released.
