#!/usr/bin/env bash
# Exercises deploy/bootstrap.sh's settings-resolution block against fake
# installed units. No podman and no systemd: that block reads a file and shell
# variables and nothing else, which is the whole reason it can be tested here.
#
# What it guards: a re-run of bootstrap.sh on a configured host must keep the
# settings that host was given. Before this existed, a bare `bash bootstrap.sh`
# rewrote the unit from built-in defaults — so a host installed with
# SLUICE_DATA_DIR=/mnt/big/data came back pointing at $SLUICE_DIR/data, started
# healthy against an empty directory, and appeared to have lost every channel.
#
#   bash scripts/test-bootstrap-resolve.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$REPO_ROOT/deploy/bootstrap.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The block under test, taken between its markers in bootstrap.sh.
sed -n '/^# >>> settings-resolution$/,/^# <<< settings-resolution$/p' "$BOOTSTRAP" > "$WORK/resolve.sh"
if [ ! -s "$WORK/resolve.sh" ]; then
	echo "could not find the settings-resolution markers in $BOOTSTRAP" >&2
	exit 1
fi

SCRATCH="$WORK"
fails=0

run_case() {
	local name="$1" unit_body="$2"; shift 2
	local tmp; tmp=$(mktemp -d)
	# Set by bootstrap.sh just above the extracted block, from UNIT_DIR.
	UNIT_DIR="$tmp"
	UNIT_FILE="$tmp/sluice.container"
	if [ -n "$unit_body" ]; then
		printf '%s\n' "$unit_body" > "$tmp/sluice.container"
	fi
	ROOTLESS=false
	SLUICE_DIR_DEFAULT=/srv/sluice
	IMAGE_REPO_DEFAULT="ghcr.io/a840817a/sluice"
	ARG_TAG="" ARG_IMAGE_REPO="" ARG_HTTP_PORT="" ARG_SLUICE_DIR="" ARG_DATA_DIR=""
	# Per-case overrides, e.g. ARG_TAG=latest
	while [ $# -gt 0 ]; do
		case "$1" in *=*) eval "$1" ;; esac
		shift
	done
	# shellcheck disable=SC1090
	. "$SCRATCH/resolve.sh" >/dev/null
	printf '%-42s image=%-38s port=%-6s data=%s sluice=%s\n' \
		"$name" "$IMAGE" "$HTTP_PORT" "$DATA_DIR" "$SLUICE_DIR"
	rm -rf "$tmp"
}

expect() { # expect <label> <got> <want>
	if [ "$2" != "$3" ]; then
		echo "  FAIL $1: got '$2' want '$3'"; fails=$((fails+1))
	fi
}

CUSTOM='[Container]
Image=ghcr.io/a840817a/sluice:0.1.0
PublishPort=9000:8080
Volume=/mnt/big/data:/data:Z
EnvironmentFile=/opt/sluice/.env'

echo "--- A: nothing installed, nothing given -> built-in defaults"
run_case "fresh install" ""
expect image "$IMAGE" "ghcr.io/a840817a/sluice:latest"
expect port "$HTTP_PORT" "8080"
expect data "$DATA_DIR" "/srv/sluice/data"
expect sluice "$SLUICE_DIR" "/srv/sluice"

echo "--- B: custom install, bare re-run -> everything preserved"
run_case "bare re-run over custom host" "$CUSTOM"
expect image "$IMAGE" "ghcr.io/a840817a/sluice:0.1.0"
expect port "$HTTP_PORT" "9000"
expect data "$DATA_DIR" "/mnt/big/data"
expect sluice "$SLUICE_DIR" "/opt/sluice"

echo "--- C: explicit 'latest' moves the pin, rest preserved"
run_case "bootstrap.sh latest" "$CUSTOM" ARG_TAG=latest
expect image "$IMAGE" "ghcr.io/a840817a/sluice:latest"
expect data "$DATA_DIR" "/mnt/big/data"

echo "--- D: one setting named, others preserved"
run_case "SLUICE_HTTP_PORT=7000" "$CUSTOM" ARG_HTTP_PORT=7000
expect port "$HTTP_PORT" "7000"
expect data "$DATA_DIR" "/mnt/big/data"
expect image "$IMAGE" "ghcr.io/a840817a/sluice:0.1.0"

echo "--- E: installed :latest still upgrades on a bare re-run"
run_case "moving tag" "$(printf '%s\n' 'Image=ghcr.io/a840817a/sluice:latest' 'PublishPort=8080:8080' 'Volume=/srv/sluice/data:/data' 'EnvironmentFile=/srv/sluice/.env')"
expect image "$IMAGE" "ghcr.io/a840817a/sluice:latest"

echo "--- F: PublishPort carrying a bind address"
run_case "bind address in PublishPort" "$(printf '%s\n' 'Image=ghcr.io/a840817a/sluice:latest' 'PublishPort=1.2.3.4:9000:8080' 'Volume=/srv/sluice/data:/data' 'EnvironmentFile=/srv/sluice/.env')"
expect port "$HTTP_PORT" "9000"

echo "--- G: registry with a port and no tag"
run_case "registry port, untagged" "$(printf '%s\n' 'Image=registry.local:5000/sluice' 'PublishPort=8080:8080' 'Volume=/srv/sluice/data:/data' 'EnvironmentFile=/srv/sluice/.env')"
expect image "$IMAGE" "registry.local:5000/sluice:latest"

echo
if [ "$fails" -eq 0 ]; then echo "ALL CASES PASS"; else echo "$fails CHECKS FAILED"; exit 1; fi
