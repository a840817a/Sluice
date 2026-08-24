# Deploying Sluice

Two launchers, one image and one `.env` between them:

| File | Engine | Notes |
|---|---|---|
| `../compose.yaml` | Docker | Verified working |
| `sluice.container` | Podman + systemd (Quadlet) | Container behaviour verified; the unit itself is parsed, not yet started |
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

Skipping this on a rootful host produces a permission error from deep inside the
segment store rather than at startup. It was the first failure found when this
was run on a real Podman host.

The gateway refuses to start with its built-in password on any address that is
not loopback, so this is not optional.

## Upgrading

Edit the `Image=` tag, then:

```bash
systemctl --user daemon-reload
systemctl --user restart sluice
```

Rolling back is the same edit with the previous tag. Every push to `main` also
publishes an immutable `sha-<short>` tag if you need to pin something that was
never released.
