#!/usr/bin/env bash
# Install and start Sluice as a Podman + systemd service.
#
#   ./deploy/install-quadlet.sh                 # ghcr.io/a840817a/sluice:latest
#   ./deploy/install-quadlet.sh 0.1.1           # a pinned tag
#   SLUICE_DIR=/srv/sluice ./deploy/install-quadlet.sh
#
# Everything it does by hand is something that was got wrong at least once while
# this was being built: the group-write bit rootful needs, the keep-id line
# rootless needs and rootful rejects, the SELinux relabel, and a health command
# that has to be a JSON array because the image has no shell.
set -euo pipefail

TAG="${1:-latest}"
IMAGE="${IMAGE:-ghcr.io/a840817a/sluice}:${TAG}"
SLUICE_DIR="${SLUICE_DIR:-$HOME/sluice}"
SRC="$(cd "$(dirname "$0")" && pwd)/sluice.container"

command -v podman >/dev/null || { echo "podman not found"; exit 1; }
[ -f "$SRC" ] || { echo "missing $SRC"; exit 1; }

ROOTLESS=$(podman info --format '{{.Host.Security.Rootless}}')
SELINUX=$(podman info --format '{{.Host.Security.SELinuxEnabled}}')

if [ "$ROOTLESS" = "true" ]; then
	UNIT_DIR="$HOME/.config/containers/systemd"
	SYSTEMCTL=(systemctl --user)
else
	UNIT_DIR="/etc/containers/systemd"
	SYSTEMCTL=(systemctl)
	[ "$(id -u)" -eq 0 ] || { echo "rootful Podman: run this as root"; exit 1; }
fi

echo "podman   rootless=$ROOTLESS selinux=$SELINUX"
echo "image    $IMAGE"
echo "state    $SLUICE_DIR"
echo "unit     $UNIT_DIR/sluice.container"

mkdir -p "$SLUICE_DIR/data" "$UNIT_DIR"

# Rootful runs the container as uid 65532 group 0 against a directory this
# script just created as root, so group-write is what makes it writable.
# Rootless does not need it: keep-id maps the caller onto 65532, and the
# directory keeps belonging to them.
if [ "$ROOTLESS" != "true" ]; then
	chmod 0775 "$SLUICE_DIR/data"
fi

# --- .env -----------------------------------------------------------------
ENV_FILE="$SLUICE_DIR/.env"
if [ ! -f "$ENV_FILE" ]; then
	printf 'SLUICE_ADMIN_PASSWORD=\n' > "$ENV_FILE"
	chmod 0600 "$ENV_FILE"
fi
if ! grep -qE '^SLUICE_ADMIN_PASSWORD=.+' "$ENV_FILE"; then
	# Not generated for you: a password this script invented would be one nobody
	# recorded, and the gateway refuses the built-in one off loopback anyway.
	echo
	echo "Set a password first — the gateway will not start without one:"
	echo "    \$EDITOR $ENV_FILE      # SLUICE_ADMIN_PASSWORD="
	exit 1
fi

# --- unit -----------------------------------------------------------------
tmp=$(mktemp)
sed -e "s#^Image=.*#Image=${IMAGE}#" \
    -e "s#%h/sluice#${SLUICE_DIR}#g" "$SRC" > "$tmp"

# keep-id is rootless-only; rootful Podman rejects the whole unit over it.
if [ "$ROOTLESS" != "true" ]; then
	sed -i.bak '/^UserNS=keep-id/d' "$tmp" && rm -f "$tmp.bak"
fi

# :Z relabels for SELinux. Without it the mount is denied, and the error names
# permissions rather than the label, which is why this is not left to the reader.
if [ "$SELINUX" != "true" ]; then
	sed -i.bak 's#^\(Volume=.*\):Z$#\1#' "$tmp" && rm -f "$tmp.bak"
fi

install -m 0644 "$tmp" "$UNIT_DIR/sluice.container"
rm -f "$tmp"

"${SYSTEMCTL[@]}" daemon-reload
"${SYSTEMCTL[@]}" restart sluice

echo
"${SYSTEMCTL[@]}" --no-pager --lines=0 status sluice || true
echo
echo "  podman ps                       # expect (healthy) within ~10s"
echo "  curl -s localhost:8080/healthz"
echo "  ${SYSTEMCTL[*]} stop sluice"
