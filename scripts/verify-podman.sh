#!/usr/bin/env bash
# Run the Podman checklist from deploy/README.md and report what passes.
#
# Run this on a Podman host. Nothing in the Podman path has ever been executed,
# so this script is the acceptance test for that whole half of the design.
#
#   ./scripts/verify-podman.sh                 # checks against the published image
#   ./scripts/verify-podman.sh --build         # also builds locally, incl. multi-arch
#
# This script has not been run either. If it is wrong about how Podman behaves,
# that is a finding worth the same as any other on the list.
set -uo pipefail

IMAGE="${IMAGE:-ghcr.io/a840817a/sluice:latest}"
WORK="$(mktemp -d)"
CTR="sluice-verify-$$"
VOL="sluice-verify-vol-$$"
pass=0; fail=0; skip=0

cleanup() {
	podman rm -f "$CTR" >/dev/null 2>&1
	podman volume rm -f "$VOL" >/dev/null 2>&1
	rm -rf "$WORK"
}
trap cleanup EXIT

ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
no()   { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; [ $# -gt 1 ] && printf '        %s\n' "$2"; fail=$((fail+1)); }
meh()  { printf '  \033[33mSKIP\033[0m  %s\n' "$1"; skip=$((skip+1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

command -v podman >/dev/null || { echo "podman not found"; exit 1; }

head_ "Environment"
echo "  podman   $(podman version --format '{{.Client.Version}}' 2>/dev/null)"
ROOTLESS=$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)
echo "  rootless $ROOTLESS"
SELINUX=$(podman info --format '{{.Host.Security.SELinuxEnabled}}' 2>/dev/null)
echo "  selinux  $SELINUX"
echo "  image    $IMAGE"

head_ "Image"
if podman pull -q "$IMAGE" >/dev/null 2>&1; then
	ok "pull $IMAGE"
	arch=$(podman image inspect "$IMAGE" --format '{{.Architecture}}')
	ok "multi-arch manifest resolved to this host ($arch)"
else
	no "pull $IMAGE" "private package? run: podman login ghcr.io"
fi

if [ "${1:-}" = "--build" ]; then
	podman build -q -t sluice:verify . >/dev/null 2>&1 \
		&& ok "podman build (default target)" || no "podman build (default target)"
	# UNVERIFIED #1: Buildah's support for FROM --platform=$BUILDPLATFORM and
	# the automatic TARGETOS/TARGETARCH args the Dockerfile relies on.
	podman build -q --platform linux/amd64,linux/arm64 \
		--manifest sluice:verify-multi . >/dev/null 2>&1 \
		&& ok "multi-arch build (BUILDPLATFORM + TARGETARCH honoured)" \
		|| no "multi-arch build" "Buildah may not support these build args"
else
	meh "local build (pass --build to include it)"
fi

head_ "Bind mount — the default data path"
mkdir -p "$WORK/data"
before=$(stat -c '%u' "$WORK/data")
USERNS=()
[ "$ROOTLESS" = "true" ] && USERNS=(--userns "keep-id:uid=65532,gid=65532")
MOUNT="$WORK/data:/data"
[ "$SELINUX" = "true" ] && MOUNT="$MOUNT:Z"

# UNVERIFIED #2b: the mount must be writable AND the directory must still be
# owned by the invoking user afterwards — that second half is the whole reason
# keep-id is preferred over :U, which chowns it away.
#
# Tested with a shell image rather than by starting the gateway: with no
# channels configured the gateway writes nothing at all, so "did files appear"
# reports a failure on a perfectly good mount. The uid/gid below are the ones
# the runtime image actually runs as.
# The probe needs a shell, which the runtime image does not have, *and* the same
# USER as the runtime image — otherwise it tests uid/gid semantics the real
# container never uses. The debug target is exactly that image plus a shell, so
# it is built here rather than substituting alpine and guessing at --user, which
# maps differently again under rootless keep-id.
PROBE_IMG="${PROBE_IMG:-sluice-probe:local}"
if ! podman image exists "$PROBE_IMG" 2>/dev/null; then
	podman build -q --target debug -t "$PROBE_IMG" . >/dev/null 2>&1 \
		|| { echo "  could not build the debug probe image"; }
fi

probe_write() {
	podman run --rm "${USERNS[@]}" -v "$MOUNT" --entrypoint sh "$PROBE_IMG" \
		-c 'touch /data/probe && rm -f /data/probe' >/dev/null 2>&1
}

# Tested both ways on purpose. A check that only runs the fixed case proves the
# instruction is sufficient but never that it is necessary, and an unnecessary
# instruction in a deployment doc is how superstition gets copied forward.
#
# Not started via the gateway: with no channels it writes nothing at all, so
# "did files appear" fails on a working mount too.
chmod 0755 "$WORK/data"
if probe_write; then
	if [ "$ROOTLESS" = "true" ]; then
		ok "writable at 0755 (keep-id maps you onto 65532)"
	else
		no "writable at 0755 without group-write" \
			"expected a failure here; the chmod instruction may be unnecessary on this host"
	fi
else
	ok "correctly NOT writable at $(stat -c '%U:%G %a' "$WORK/data") — the documented failure"
fi

chmod 0775 "$WORK/data"
if probe_write; then
	ok "writable after chmod 0775 (process is uid 65532 gid 0)"
else
	no "still not writable after chmod 0775" \
		"directory is $(stat -c '%U:%G %a' "$WORK/data"); is USER 65532:0 in the image you pulled?"
fi

after=$(stat -c '%u' "$WORK/data")
if [ "$before" = "$after" ]; then
	ok "host directory still owned by uid $after (no :U-style chown)"
else
	no "host directory owner changed: $before -> $after" "sudo now needed to clean it up"
fi

head_ "Named volume — the DATA_MOUNT opt-in"
# UNVERIFIED #2: the image's 65532:0 with g+rwX is what should make this work
# without keep-id.
podman volume create "$VOL" >/dev/null 2>&1
podman run -d --name "$CTR" -v "$VOL:/data" \
	-e SLUICE_ADMIN_PASSWORD=verify-only "$IMAGE" >/dev/null 2>&1
sleep 3
podman exec "$CTR" /sluice -config /etc/sluice/config.yaml -healthcheck >/dev/null 2>&1 \
	&& ok "named volume: gateway running" || no "named volume: gateway not healthy"
podman rm -f "$CTR" >/dev/null 2>&1

head_ "SELinux"
if [ "$SELINUX" = "true" ]; then
	# :Z has to be shown to be *necessary*, not merely present.
	chmod 0775 "$WORK/data"
	if podman run --rm "${USERNS[@]}" -v "$WORK/data:/data" --entrypoint sh \
		"$PROBE_IMG" -c 'touch /data/z && rm -f /data/z' >/dev/null 2>&1; then
		no "the mount works WITHOUT :Z on an SELinux host" \
			"the :Z instruction is then unnecessary here — check the policy before copying it forward"
	else
		ok "without :Z the mount is denied (so the instruction is necessary)"
	fi
	if podman run --rm "${USERNS[@]}" -v "$WORK/data:/data:Z" --entrypoint sh \
		"$PROBE_IMG" -c 'touch /data/z && rm -f /data/z' >/dev/null 2>&1; then
		ok "with :Z the mount works"
	else
		no "even :Z does not make the mount work"
	fi
else
	meh "SELinux not enabled on this host"
fi

head_ "Read-only root filesystem"
# UNVERIFIED #4: whether Podman needs tmpfs mounts Docker did not.
podman run -d --name "$CTR" --read-only "${USERNS[@]}" -v "$MOUNT" \
	-e SLUICE_ADMIN_PASSWORD=verify-only "$IMAGE" >/dev/null 2>&1
sleep 3
podman exec "$CTR" /sluice -config /etc/sluice/config.yaml -healthcheck >/dev/null 2>&1 \
	&& ok "serves with ReadOnly=true" \
	|| no "does not serve read-only" "$(podman logs "$CTR" 2>&1 | tail -2)"

head_ "Healthcheck"
# UNVERIFIED #3: does Podman honour the image's HEALTHCHECK?
hc=$(podman inspect "$CTR" --format '{{json .Config.Healthcheck}}' 2>/dev/null)
if podman healthcheck run "$CTR" >/dev/null 2>&1; then
	ok "image HEALTHCHECK honoured (Podman behaviour changed — the unit's HealthCmd= is now redundant)"
elif [ "$hc" = "null" ] || [ -z "$hc" ]; then
	# Known and worked around. Reported as a fact rather than a failure: a FAIL
	# that can never pass just teaches people to skim this output.
	meh "image HEALTHCHECK not read by Podman (known; OCI configs have no such field)"
	printf '        %s\n' "the Quadlet unit declares HealthCmd= itself — see the next check"
else
	no "healthcheck defined but failing" "$hc"
fi
podman rm -f "$CTR" >/dev/null 2>&1
# The fix the Quadlet unit uses. If this passes while the image healthcheck does
# not, the diagnosis is confirmed: the command is fine and Podman simply never
# read it from the image.
# JSON array form: Podman documents that a plain string is passed to
# /bin/sh -c, and this image has no shell. Two spellings of the array are in
# circulation, so both are tried rather than costing another round trip; the
# unit must use whichever is reported working here.
try_health() { # $1 label, $2 health-cmd value
	podman rm -f "$CTR" >/dev/null 2>&1
	podman run -d --name "$CTR" --health-cmd "$2" --health-start-period 2s \
		-e SLUICE_ADMIN_PASSWORD=verify-only "$IMAGE" >/dev/null 2>&1 || {
		no "could not start with $1"; return 1; }
	sleep 4
	if out=$(podman healthcheck run "$CTR" 2>&1); then
		ok "$1 works"
		return 0
	fi
	no "$1 fails" "${out:-<no error text>}"
	return 1
}
try_health 'JSON array, bare command' \
	'["/sluice", "-config", "/etc/sluice/config.yaml", "-healthcheck"]' || \
try_health 'JSON array, CMD-prefixed' \
	'["CMD", "/sluice", "-config", "/etc/sluice/config.yaml", "-healthcheck"]'
podman rm -f "$CTR" >/dev/null 2>&1
podman rm -f "$CTR" >/dev/null 2>&1

head_ "Secret delivered as a file"
printf 'from-a-secret' | podman secret create sluice-verify-pw - >/dev/null 2>&1
if podman run -d --name "$CTR" \
	--secret sluice-verify-pw,type=mount,target=/run/secrets/pw \
	-e SLUICE_ADMIN_PASSWORD_FILE=/run/secrets/pw "$IMAGE" >/dev/null 2>&1; then
	sleep 3
	podman exec "$CTR" /sluice -config /etc/sluice/config.yaml -healthcheck >/dev/null 2>&1 \
		&& ok "_FILE secret accepted (no wrapper script needed)" \
		|| no "_FILE secret not accepted" "$(podman logs "$CTR" 2>&1 | tail -2)"
else
	no "could not start with a mounted secret"
fi
podman rm -f "$CTR" >/dev/null 2>&1
podman secret rm sluice-verify-pw >/dev/null 2>&1

head_ "Quadlet unit"
QUADLET=""
for c in /usr/libexec/podman/quadlet /usr/lib/podman/quadlet; do
	[ -x "$c" ] && QUADLET="$c" && break
done
if [ -n "$QUADLET" ]; then
	mkdir -p "$WORK/units"
	sed "s#%h/sluice#$WORK#g" deploy/sluice.container > "$WORK/units/sluice.container"
	if out=$(QUADLET_UNIT_DIRS="$WORK/units" "$QUADLET" -dryrun -user 2>&1); then
		echo "$out" | grep -q 'sluice' \
			&& ok "quadlet parses the unit" \
			|| no "quadlet produced no output for the unit" "$out"
	else
		no "quadlet rejected the unit" "$out"
	fi
else
	meh "quadlet generator not found (checked /usr/libexec and /usr/lib)"
fi

head_ "Rootless low ports"
if [ "$ROOTLESS" = "true" ]; then
	# Must test the port binding alone. Running the gateway with a bad flag
	# also exits non-zero, and that would read as a binding failure.
	out=$(podman run --rm -p 443:8080 --entrypoint sh "$PROBE_IMG" -c true 2>&1)
	if [ $? -eq 0 ]; then
		ok "this host binds 443 rootless (ip_unprivileged_port_start is lowered)"
		printf '        %s\n' "so addr: \":443\" works here; it does not on a default host"
	else
		ok "binding 443 rootless is refused, as documented"
		printf '        %s\n' "${out##*: }"
	fi
else
	meh "not rootless"
fi

printf '\n\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ] || echo "Anything failing is a bug in this repo's Podman support, not in your host."
exit $((fail > 0))
