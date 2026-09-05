#!/usr/bin/env bash
# Install and start Sluice under Podman + systemd on a host with no checkout.
#
#   curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo bash
#   curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo bash -s -- 0.1.1
#   sudo SLUICE_DIR=/srv/sluice SLUICE_ADMIN_PASSWORD=... bash bootstrap.sh
#
# Unlike deploy/install-quadlet.sh this carries the unit inline, so nothing is
# read from the repository — that is the point of it, and why it stays runnable
# on its own after a curl. The two must be kept in step: if sluice.container
# changes, change the heredoc below too.
set -euo pipefail

# :latest by default so a re-run is an upgrade. Pass a tag to pin — a host you
# want to be predictable should pin, and rolling back is then one argument.
TAG="${1:-latest}"
IMAGE="${IMAGE:-ghcr.io/a840817a/sluice}:${TAG}"
HTTP_PORT="${SLUICE_HTTP_PORT:-8080}"

# TLS is off unless asked for. It matters for more than eavesdropping: browsers
# expose EME only in a secure context, so a DRM channel served over plain HTTP
# to anything but localhost fails in the player with shaka error 6020
# (MISSING_EME_SUPPORT) — navigator.requestMediaKeySystemAccess is simply not
# there. Clear content is unaffected.
TLS_SELF_SIGNED="${SLUICE_TLS_SELF_SIGNED:-false}"
TLS_IP="${SLUICE_TLS_IP:-}"
TLS_GIVEN=""
[ -n "${SLUICE_TLS_SELF_SIGNED:-}${SLUICE_TLS_IP:-}" ] && TLS_GIVEN=yes

command -v podman >/dev/null || { echo "podman not found"; exit 1; }
command -v systemctl >/dev/null || { echo "systemd not found — this installs a Quadlet unit"; exit 1; }

# Quadlet arrived in Podman 4.4. Fail here rather than with a unit that is
# silently never generated, which looks like "systemctl says no such service".
ver=$(podman version --format '{{.Client.Version}}')
case "$ver" in
	[0-3].*|4.[0-3].*) echo "podman $ver is too old for Quadlet (need >= 4.4)"; exit 1 ;;
esac

ROOTLESS=$(podman info --format '{{.Host.Security.Rootless}}')
SELINUX=$(podman info --format '{{.Host.Security.SELinuxEnabled}}')

if [ "$ROOTLESS" = "true" ]; then
	SLUICE_DIR="${SLUICE_DIR:-$HOME/sluice}"
	UNIT_DIR="$HOME/.config/containers/systemd"
	SYSTEMCTL=(systemctl --user)
else
	SLUICE_DIR="${SLUICE_DIR:-/srv/sluice}"
	UNIT_DIR="/etc/containers/systemd"
	SYSTEMCTL=(systemctl)
	[ "$(id -u)" -eq 0 ] || { echo "rootful podman: run this as root"; exit 1; }
fi

# Rootless below 1024 is refused by pasta with EPERM, and the error names the
# port rather than the reason.
if [ "$ROOTLESS" = "true" ] && [ "$HTTP_PORT" -lt 1024 ]; then
	echo "rootless podman cannot publish port $HTTP_PORT (<1024)"; exit 1
fi

# Segments only grow (store.cleanup defaults to "disabled"), so the data
# directory is the one thing worth putting on its own disk. Defaults to living
# under SLUICE_DIR, which is right for a single-disk host.
DATA_DIR="${SLUICE_DATA_DIR:-$SLUICE_DIR/data}"

# A self-signed certificate covers every address the gateway can see, but it
# runs in a network namespace: net.InterfaceAddrs() returns the container's
# addresses, never the host's. So the address people actually type has to be
# passed in, or the certificate will not carry it — and a name mismatch is the
# one TLS warning a browser will not let you click through.
if [ "$TLS_SELF_SIGNED" = "true" ] && [ -z "$TLS_IP" ]; then
	TLS_IP=$(ip -4 route get 1.1.1.1 2>/dev/null |
		awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}')
	[ -n "$TLS_IP" ] && echo "note     SLUICE_TLS_IP not set; using $TLS_IP for the certificate"
fi

echo "podman   $ver rootless=$ROOTLESS selinux=$SELINUX"
echo "image    $IMAGE"
echo "state    $SLUICE_DIR"
echo "data     $DATA_DIR"
echo "unit     $UNIT_DIR/sluice.container"

mkdir -p "$SLUICE_DIR" "$DATA_DIR" "$UNIT_DIR"

# Rootful runs the container as 65532:0 against a root-owned directory, so
# group-write is what makes it writable. Rootless does not need it: keep-id
# maps the caller onto 65532 and the directory keeps belonging to them.
[ "$ROOTLESS" = "true" ] || chmod 0775 "$DATA_DIR"

# --- password -------------------------------------------------------------
# Never generated for you: an invented password is one nobody recorded, and the
# gateway refuses its built-in one on any non-loopback address anyway.
ENV_FILE="$SLUICE_DIR/.env"
if [ ! -f "$ENV_FILE" ]; then
	if [ -n "${SLUICE_ADMIN_PASSWORD:-}" ]; then
		pw="$SLUICE_ADMIN_PASSWORD"
	elif [ -t 0 ]; then
		read -r -s -p "Admin password: " pw; echo
	else
		echo "no $ENV_FILE and no SLUICE_ADMIN_PASSWORD; stdin is not a terminal" >&2
		echo "  re-run with: SLUICE_ADMIN_PASSWORD=... $0 $TAG" >&2
		exit 1
	fi
	[ -n "$pw" ] || { echo "empty password"; exit 1; }
	( umask 077; printf 'SLUICE_ADMIN_PASSWORD=%s\n' "$pw" > "$ENV_FILE" )
	unset pw
	echo "wrote    $ENV_FILE"
elif ! grep -qE '^SLUICE_ADMIN_PASSWORD=.+' "$ENV_FILE"; then
	echo "$ENV_FILE exists but SLUICE_ADMIN_PASSWORD is empty — set it and re-run"; exit 1
else
	echo "kept     $ENV_FILE (existing password left alone)"
fi

# --- TLS ------------------------------------------------------------------
# Same .env as the password: one per-host file, and the unit reads it already.
# Only rewritten when this run was actually given TLS settings.
set_env() {
	if grep -qE "^$1=" "$ENV_FILE"; then
		sed -i "s#^$1=.*#$1=$2#" "$ENV_FILE"
	else
		printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
	fi
}
# Each key is written only if this run named it. Writing both together would
# mean that re-running with just a new SLUICE_TLS_IP — to correct an address —
# also wrote SELF_SIGNED=false and silently turned TLS off.
if [ -n "${SLUICE_TLS_SELF_SIGNED:-}" ]; then
	set_env SLUICE_TLS_SELF_SIGNED "$TLS_SELF_SIGNED"
fi
if [ -n "$TLS_IP" ] && [ -n "$TLS_GIVEN" ]; then
	set_env SLUICE_TLS_IP "$TLS_IP"
fi
[ -n "$TLS_GIVEN" ] && echo "wrote    TLS settings into $ENV_FILE"

# From here the file is the source of truth, so a host configured by an earlier
# run is described and probed correctly by a re-run that says nothing about TLS.
if grep -qE '^SLUICE_TLS_SELF_SIGNED=true' "$ENV_FILE"; then
	TLS_SELF_SIGNED=true
	SCHEME=https
	TLS_IP=$(sed -n 's/^SLUICE_TLS_IP=//p' "$ENV_FILE")
	if [ -n "$TLS_IP" ]; then
		echo "tls      self-signed, certificate covers $TLS_IP"
	else
		echo "tls      self-signed, but SLUICE_TLS_IP is unset"
		echo "warn     the certificate will not carry this host's address, and a name"
		echo "warn     mismatch is the one TLS warning a browser cannot click through"
	fi
else
	TLS_SELF_SIGNED=false
	SCHEME=http
	echo "tls      off (plain HTTP)"
fi

# :Z relabels the mount for SELinux. Harmless where SELinux is off, but dropped
# anyway so the preflight below and the installed unit both reflect the host.
VOL_SUFFIX=":Z"
[ "$SELINUX" = "true" ] || VOL_SUFFIX=""

# --- image ----------------------------------------------------------------
# Pulled here rather than left to the unit's first start, for two reasons. The
# preflight below runs the image, so it has to be present; and Podman's default
# pull policy is "missing", so with a moving tag like :latest a re-run would
# otherwise keep starting whatever is already on disk. That is what makes
# re-running this script an upgrade.
echo "pulling  $IMAGE"
podman pull -q "$IMAGE" >/dev/null

# --- preflight: can the gateway actually write to this directory? ---------
#
# Run inside the real image, with the real mount, the real user namespace and
# the real SELinux label, before a unit exists. Everything that makes /data
# unusable answers here: a mode or owner the process does not match, an image
# that declares USER without a group (its primary gid then comes from its own
# /etc/passwd, not 0, and a group-0-writable directory buys nothing), a missing
# :Z relabel, a read-only mount.
#
# It runs the same store.CheckWritable the gateway calls at startup, so there is
# one definition of "writable" rather than a shell reimplementation of it that
# drifts. Doing it here as well is about timing, not coverage: the unit is
# Restart=always, so installing first would leave a service crash-looping and at
# boot, for a fault that is already knowable.
#
# No password is set at this point and none is needed — -check-store runs ahead
# of the admin-password policy, which cmd/gateway/checkstore_test.go pins.
echo -n "checking $DATA_DIR ... "
preflight=(podman run --rm --read-only -v "${DATA_DIR}:/data${VOL_SUFFIX}")
[ "$ROOTLESS" = "true" ] && preflight+=(--userns=keep-id:uid=65532,gid=65532)
preflight+=("$IMAGE" -config /etc/sluice/config.yaml -check-store)

if out=$("${preflight[@]}" 2>&1); then
	echo "writable"
elif printf '%s' "$out" | grep -q 'flag provided but not defined'; then
	# -check-store arrived after 0.1.1, so an older tag rejects it before doing
	# anything. Continue rather than refuse: those images work when the volume is
	# right, and the gateway in them simply cannot say so in advance. Say plainly
	# what is not being checked instead of skipping it quietly.
	echo "unavailable"
	echo "warn     $IMAGE predates -check-store, so the volume cannot be verified"
	echo "warn     before install. 0.1.0 in particular runs as gid 65532 and cannot"
	echo "warn     write a group-0 directory — if this host misbehaves, upgrade the tag."
else
	echo "FAILED"
	{
		echo
		echo "$out"
		echo
		echo "The gateway cannot write to $DATA_DIR, so it would start, report"
		echo "healthy, and then fail every segment write. Not installing the unit."
		echo
		echo "The line above gives the directory's uid/gid/mode and the uid/gid the"
		echo "process runs as; the usual fixes are:"
		if [ "$ROOTLESS" = "true" ]; then
			echo "    chown -R \$(id -u) $DATA_DIR"
		else
			echo "    chown root:0 $DATA_DIR && chmod 0775 $DATA_DIR"
			echo "    (or use a newer image tag: before 0.1.1 the process ran as gid 65532, not 0)"
		fi
		[ "$SELINUX" = "true" ] && echo "    restorecon -Rv $DATA_DIR        # SELinux label"
	} >&2
	exit 1
fi

# --- unit -----------------------------------------------------------------
# keep-id is rootless-only; rootful podman rejects the whole unit over it.
USERNS_LINE="UserNS=keep-id:uid=65532,gid=65532"
[ "$ROOTLESS" = "true" ] || USERNS_LINE="# UserNS=keep-id omitted: rootless-only, and rootful rejects the unit over it"

tmp=$(mktemp)
cat > "$tmp" <<EOF
# Generated by bootstrap.sh on $(date -u +%FT%TZ) — edits are overwritten by a
# re-run. Source of truth: deploy/sluice.container in the Sluice repository.
#
# The tag below is whatever was installed. A moving tag (:latest) does not
# upgrade on restart — Podman pulls only what is missing — so re-run
# bootstrap.sh to upgrade, or edit this line to a pinned tag and restart.
[Unit]
Description=Sluice DASH/HLS gateway
Documentation=https://github.com/a840817a/Sluice
After=network-online.target
Wants=network-online.target

[Container]
Image=${IMAGE}
ContainerName=sluice
PublishPort=${HTTP_PORT}:8080
${USERNS_LINE}
Volume=${DATA_DIR}:/data${VOL_SUFFIX}
EnvironmentFile=${SLUICE_DIR}/.env
ReadOnly=true

# JSON array, and it must stay one: podman passes a plain string to /bin/sh -c
# and this image has no shell. Repeated here because podman does not read the
# image's own HEALTHCHECK. It probes the listener only — it says nothing about
# whether ${DATA_DIR} is writable, which bootstrap.sh checks instead.
HealthCmd=["/sluice", "-config", "/etc/sluice/config.yaml", "-healthcheck"]
HealthInterval=30s
HealthTimeout=5s
HealthStartPeriod=5s
HealthRetries=3

[Service]
Restart=always
# First start on a slow link includes pulling a multi-arch image.
TimeoutStartSec=900

[Install]
WantedBy=default.target
EOF
install -m 0644 "$tmp" "$UNIT_DIR/sluice.container"
rm -f "$tmp"

# Rootless units die with the last session unless lingering is on.
if [ "$ROOTLESS" = "true" ] && command -v loginctl >/dev/null; then
	loginctl enable-linger "$(id -un)" 2>/dev/null || true
fi

"${SYSTEMCTL[@]}" daemon-reload
"${SYSTEMCTL[@]}" restart sluice

# --- verify ---------------------------------------------------------------
# The unit is "started" the moment the container is created; healthy is the
# claim worth making, so wait for it rather than printing success early.
echo -n "waiting for healthy "
state=""
for _ in $(seq 1 60); do
	state=$(podman inspect sluice --format '{{.State.Health.Status}}' 2>/dev/null || echo "")
	[ "$state" = "healthy" ] && break
	printf .
	sleep 2
done
echo " $state"

"${SYSTEMCTL[@]}" --no-pager --lines=0 status sluice || true

if [ "$state" != "healthy" ]; then
	echo
	echo "not healthy. Look at:"
	echo "  podman logs sluice"
	echo "  ${SYSTEMCTL[*]} status sluice"
	exit 1
fi

echo
# -k when self-signed: nothing can verify a certificate the gateway generated
# for itself, and this request never leaves loopback. The question here is "is
# my own listener answering", not who it claims to be.
probe=(curl -fsS)
[ "$TLS_SELF_SIGNED" = "true" ] && probe+=(-k)
"${probe[@]}" "${SCHEME}://localhost:${HTTP_PORT}/healthz" && echo

echo "  ${SCHEME}://<this host>:${HTTP_PORT}/    # player"
echo "  ${SYSTEMCTL[*]} stop sluice        # stop"
echo "  bash bootstrap.sh                  # re-run to upgrade (:latest)"
echo "  ${SYSTEMCTL[*]} restart sluice     # after editing Image= to pin or roll back"

# Last, so it survives a curl | bash that scrolled everything else away.
if [ "$TLS_SELF_SIGNED" != "true" ]; then
	echo
	if [ "$HTTP_PORT" = "443" ]; then
		echo "NOTE: published on 443 but serving plain HTTP — https://<host>/ will not"
		echo "      connect at all. That is almost certainly not what you meant."
	fi
	echo "NOTE: no TLS. Browsers expose EME only in a secure context, so a DRM"
	echo "      channel opened at anything but localhost fails in the player with"
	echo "      shaka error 6020 (MISSING_EME_SUPPORT). Clear content is unaffected."
	echo "      To switch on a self-signed certificate, re-run with:"
	echo "          SLUICE_TLS_SELF_SIGNED=true SLUICE_TLS_IP=<this host's IP>"
fi
