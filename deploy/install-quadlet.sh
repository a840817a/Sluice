#!/usr/bin/env bash
# Install and start Sluice as a Podman + systemd service.
#
#   ./deploy/install-quadlet.sh                 # ghcr.io/a840817a/sluice:latest
#   ./deploy/install-quadlet.sh 0.1.1           # a pinned tag
#   SLUICE_DIR=/srv/sluice ./deploy/install-quadlet.sh
#
# Re-running it upgrades rather than reinstalls: the state directory and a
# pinned tag are read back from the installed unit unless this run names
# replacements, and the outgoing unit is kept as sluice.container.bak.
#
# Everything it does by hand is something that was got wrong at least once while
# this was being built: the group-write bit rootful needs, the keep-id line
# rootless needs and rootful rejects, the SELinux relabel, and a health command
# that has to be a JSON array because the image has no shell.
set -euo pipefail

# Captured before any default is applied: re-running this on a configured host
# is an upgrade, so a setting this run does not name is taken from the unit
# already installed rather than reset. Without that, running it bare on a host
# installed with SLUICE_DIR=/srv/sluice repoints Volume= and EnvironmentFile=
# at $HOME/sluice — the gateway then starts healthy against an empty directory
# and the channels appear to be gone.
ARG_TAG="${1:-}"
ARG_IMAGE_REPO="${IMAGE:-}"
ARG_SLUICE_DIR="${SLUICE_DIR:-}"

IMAGE_REPO_DEFAULT="ghcr.io/a840817a/sluice"
SLUICE_DIR_DEFAULT="$HOME/sluice"
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
UNIT_FILE="$UNIT_DIR/sluice.container"

# --- settings already on this host ----------------------------------------
# Same rule and same shape as bootstrap.sh, over the two settings this script
# has. The markers delimit what scripts/test-deploy-settings.sh extracts;
# the block reads a file and shell variables only, which is what keeps it
# testable without podman.
# >>> settings-resolution
unit_field() {
	[ -f "$UNIT_FILE" ] || return 0
	sed -n "s/^$1=//p" "$UNIT_FILE" | tail -1
}

# A tag is what follows the last colon, but only when that colon is the tag's
# and not a registry port's: ghcr.io:5000/sluice carries no tag.
image_tag() {
	case "${1##*/}" in *:*) printf '%s' "${1##*:}" ;; *) printf '%s' "" ;; esac
}
image_repo() {
	case "${1##*/}" in *:*) printf '%s' "${1%:*}" ;; *) printf '%s' "$1" ;; esac
}

INSTALLED_IMAGE=$(unit_field Image)
INSTALLED_ENVFILE=$(unit_field EnvironmentFile)
INSTALLED_SLUICE_DIR=""
[ -n "$INSTALLED_ENVFILE" ] && INSTALLED_SLUICE_DIR=$(dirname "$INSTALLED_ENVFILE")

SLUICE_DIR="${ARG_SLUICE_DIR:-${INSTALLED_SLUICE_DIR:-$SLUICE_DIR_DEFAULT}}"

INSTALLED_TAG=$(image_tag "$INSTALLED_IMAGE")
INSTALLED_REPO=$(image_repo "$INSTALLED_IMAGE")
IMAGE_REPO="${ARG_IMAGE_REPO:-${INSTALLED_REPO:-$IMAGE_REPO_DEFAULT}}"
if [ -n "$ARG_TAG" ]; then
	TAG="$ARG_TAG"
elif [ -n "$INSTALLED_TAG" ] && [ "$INSTALLED_TAG" != "latest" ]; then
	# Pinning is deliberate; a bare re-run may upgrade behind a moving tag but
	# must not quietly undo a pin.
	TAG="$INSTALLED_TAG"
	echo "kept     pinned tag $TAG (pass a tag, or 'latest', to move it)"
else
	TAG=latest
fi
IMAGE="${IMAGE_REPO}:${TAG}"
# <<< settings-resolution

[ -f "$UNIT_FILE" ] && echo "found    $UNIT_FILE — keeping settings this run did not name"

echo "podman   rootless=$ROOTLESS selinux=$SELINUX"
echo "image    $IMAGE"
echo "state    $SLUICE_DIR"
echo "unit     $UNIT_FILE"

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

# The outgoing unit is the only record of anything applied to this host by
# hand, so keep it rather than make a bad re-run unrecoverable.
if [ -f "$UNIT_FILE" ]; then
	cp -p "$UNIT_FILE" "$UNIT_FILE.bak"
	echo "backup   $UNIT_FILE.bak"
fi
install -m 0644 "$tmp" "$UNIT_FILE"
rm -f "$tmp"

"${SYSTEMCTL[@]}" daemon-reload
"${SYSTEMCTL[@]}" restart sluice

echo
"${SYSTEMCTL[@]}" --no-pager --lines=0 status sluice || true
echo
echo "  podman ps                       # expect (healthy) within ~10s"
echo "  curl -s localhost:8080/healthz"
echo "  ${SYSTEMCTL[*]} stop sluice"
