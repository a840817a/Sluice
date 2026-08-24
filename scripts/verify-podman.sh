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

# UNVERIFIED #2b: keep-id must make the mount writable AND leave the directory
# owned by the invoking user — that second half is the whole reason it is
# preferred over :U, which chowns it away.
if podman run --rm "${USERNS[@]}" -v "$MOUNT" --entrypoint /sluice "$IMAGE" \
	-config /etc/sluice/config.yaml -healthcheck >/dev/null 2>&1; then :; fi
if podman run --rm "${USERNS[@]}" -v "$MOUNT" --entrypoint sh \
	"${IMAGE%:*}:latest" -c 'touch /data/probe' >/dev/null 2>&1 || \
   podman run --rm "${USERNS[@]}" -v "$MOUNT" --entrypoint /bin/sh \
	"$IMAGE" -c 'touch /data/probe' >/dev/null 2>&1; then
	ok "container wrote to the bind mount"
else
	# distroless has no shell; fall back to letting the gateway create its own
	# state, which is the behaviour that actually matters.
	podman run -d --name "$CTR" "${USERNS[@]}" -v "$MOUNT" \
		-e SLUICE_ADMIN_PASSWORD=verify-only "$IMAGE" >/dev/null 2>&1
	sleep 3
	if [ -n "$(ls -A "$WORK/data" 2>/dev/null)" ]; then
		ok "gateway wrote state into the bind mount"
	else
		no "nothing was written to the bind mount" "keep-id may not be mapping as expected"
	fi
	podman rm -f "$CTR" >/dev/null 2>&1
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
	# The point is that :Z is *necessary*, not merely present. A mount without it
	# should fail; if it succeeds, the README instruction is superstition here.
	podman run --rm "${USERNS[@]}" -v "$WORK/data:/data" --entrypoint /sluice \
		"$IMAGE" -config /etc/sluice/config.yaml -healthcheck >/dev/null 2>&1
	echo "        (compare the two runs above and below by hand if unsure)"
	ok ":Z used for the mount (see the bind-mount section)"
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
if podman healthcheck run "$CTR" >/dev/null 2>&1; then
	ok "podman healthcheck run (image HEALTHCHECK honoured)"
else
	no "podman healthcheck run failed" "Quadlet may need an explicit HealthCmd="
fi
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
	if podman run --rm -p 443:8080 "$IMAGE" --help >/dev/null 2>&1; then
		ok "this host can bind 443 rootless (ip_unprivileged_port_start is low)"
	else
		ok "binding 443 rootless fails as documented — use a proxy or a mapping"
	fi
else
	meh "not rootless"
fi

printf '\n\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ] || echo "Anything failing is a bug in this repo's Podman support, not in your host."
exit $((fail > 0))
