# Deploying Sluice

Two launchers, one image and one `.env` between them:

| File | Engine | Notes |
|---|---|---|
| `../compose.yaml` | Docker | Verified working |
| `sluice.container` | Podman + systemd (Quadlet) | **Not tested** — see below |
| `config.container.yaml` | both | Baked into the image; not deployed by hand |

## Podman: what must be checked before trusting it

Run it rather than reading it:

```bash
./scripts/verify-podman.sh            # against the published image
./scripts/verify-podman.sh --build    # also builds locally, including multi-arch
```

That script reports PASS/FAIL for each item below. It has not been run either —
if it is wrong about how Podman behaves, that is a finding worth as much as any
other here.

`sluice.container` was written from documentation, on a machine with no Podman.
It has never been started. Until the list below passes on a real host, the
Podman path is unverified — and a deployment file that looks finished is more
dangerous than one that is obviously missing, so the list is here rather than in
a commit message.

- [ ] `podman build` succeeds; `--platform linux/amd64,linux/arm64 --manifest`
      produces both architectures. Confirms Buildah supports the
      `FROM --platform=$BUILDPLATFORM` and `TARGETOS`/`TARGETARCH` the
      Dockerfile relies on.
- [ ] **Rootless with `UserNS=keep-id`**: the container writes segments to
      `~/sluice/data`, and afterwards that directory is still owned by you —
      readable and deletable without `sudo`. This is the default path, so it is
      the one that has to work.
- [ ] Rootful (no `UserNS=` line) also writes successfully.
- [ ] `DATA_MOUNT` pointed at a named volume is writable too, which is what the
      image's `65532:0` ownership with `g+rwX` exists for.
- [ ] On SELinux, the mount works with `:Z` and **fails without it** — check
      both, so the instruction is known to be necessary rather than copied.
- [ ] `podman healthcheck run sluice` reports healthy, confirming Podman honours
      the image's `HEALTHCHECK`.
- [ ] `ReadOnly=true` starts and serves, with no extra tmpfs needed.
- [ ] `systemctl --user restart sluice` recovers; logs reach journald.
- [ ] A password delivered by `podman secret` is picked up through
      `SLUICE_ADMIN_PASSWORD_FILE`.
- [ ] Rootless cannot bind ports below 1024: confirm the failure mode before
      someone meets it in production.

Anything that fails is a bug in this unit, not in the host. Please fix it here.

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
