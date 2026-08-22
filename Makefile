# Local and CI run the same commands from here, so the two cannot drift.
#
# Written for GNU Make 3.81, which is what macOS ships. No 4.x-only syntax
# ($(file ...), .ONESHELL): using it would break the machine this is mainly
# used on.

ENGINE      ?= docker
IMAGE       ?= sluice
TAG         ?= dev
PLATFORMS   ?= linux/amd64,linux/arm64
BUILDER     ?= sluice-builder

# Docker for Linux needs the process to run as the owner of the bind-mounted
# ./data; Docker Desktop for macOS maps ownership and does not. Passing the
# host uid is harmless on macOS, so it is unconditional rather than guessed.
SLUICE_UID  ?= $(shell id -u)
SLUICE_GID  ?= 0
export SLUICE_UID
export SLUICE_GID

.PHONY: help build test verify image image-debug image-multiarch \
        up up-tls down logs clean

help:
	@echo 'Go:        build test verify'
	@echo 'Image:     image image-debug image-multiarch     (ENGINE=docker|podman)'
	@echo 'Run:       up up-tls down logs'
	@echo 'Other:     clean'

# --- Go -------------------------------------------------------------------

build:
	go build ./...

test:
	go test -race ./...

# The four-check pass. gofmt is checked, not applied: a target that rewrites
# files cannot also be the thing CI fails on.
verify:
	go build ./...
	go vet ./...
	@out=`gofmt -l .`; \
	if [ -n "$$out" ]; then echo "gofmt needs to run on:"; echo "$$out"; exit 1; fi
	go test -race -count=1 ./...

# --- Images ---------------------------------------------------------------

image:
	$(ENGINE) build -t $(IMAGE):$(TAG) .

image-debug:
	$(ENGINE) build --target debug -t $(IMAGE):$(TAG)-debug .

# Multi-arch cannot share one command line between the engines: Docker needs a
# docker-container builder (its default driver refuses multi-platform outright)
# while Podman writes a manifest list instead. A templated engine name would
# silently do the wrong thing on one of them.
image-multiarch:
ifeq ($(ENGINE),podman)
	podman build --platform $(PLATFORMS) --manifest $(IMAGE):$(TAG) .
else
	@$(ENGINE) buildx inspect $(BUILDER) >/dev/null 2>&1 || \
		$(ENGINE) buildx create --name $(BUILDER) --driver docker-container --bootstrap
	$(ENGINE) buildx build --builder $(BUILDER) --platform $(PLATFORMS) \
		-t $(IMAGE):$(TAG) $(BUILDX_OUTPUT) .
endif

# --- Run ------------------------------------------------------------------

up: .env
	$(ENGINE) compose up -d

# HTTPS with a self-signed certificate, for testing DRM from another device:
# EME needs a secure context, and http://<LAN-IP> is not one.
#
# The address is resolved on the host because that is the only place the answer
# is right — a container sees its own bridge address, not this machine's.
up-tls: .env
	@ip="$(HOST_IP)"; \
	if [ -z "$$ip" ]; then ip=`./scripts/host-ip.sh` || exit 1; fi; \
	echo "self-signed certificate will cover $$ip"; \
	SLUICE_TLS_SELF_SIGNED=true SLUICE_TLS_IP="$$ip" SLUICE_SERVER_ADDR=":8443" \
		$(ENGINE) compose up -d --force-recreate; \
	echo "https://$$ip:$${SLUICE_HTTPS_PORT:-8443}/  (the browser will warn: the certificate is self-signed)"

down:
	$(ENGINE) compose down

logs:
	$(ENGINE) compose logs -f

# Refuse to run without .env rather than starting with a surprise password.
.env:
	@echo 'No .env. Copy the example and set a password:'
	@echo '    cp .env.example .env'
	@echo '    $$EDITOR .env      # SLUICE_ADMIN_PASSWORD must not be empty'
	@exit 1

clean:
	$(ENGINE) compose down -v 2>/dev/null || true
	rm -f sluice
