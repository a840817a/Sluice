# syntax=docker/dockerfile:1

# --- frontend: not built today -------------------------------------------
#
# The admin UI and player are hand-written files under internal/web/static,
# embedded by internal/web/embed.go. There is no package.json and no bundler,
# so a Node stage would add ~200 MB to the build for nothing.
#
# When a framework arrives, uncomment this stage and the COPY marked below.
# Both must stay BEFORE `go build`: //go:embed only sees files that exist at
# compile time, and only under internal/web/.
#
# FROM node:22-alpine AS frontend
# WORKDIR /app
# COPY frontend/package*.json ./
# RUN npm ci
# COPY frontend/ ./
# RUN npm run build

# --- builder --------------------------------------------------------------
#
# Pinned to BUILDPLATFORM so it runs natively and cross-compiles, rather than
# emulating the target under QEMU. That is only possible because the binary is
# cgo-free; if cgo is ever enabled this stage has to change.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
WORKDIR /src

# Dependencies before source, so this layer survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# COPY --from=frontend /app/dist ./internal/web/static

ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/sluice ./cmd/gateway

# The runtime image has no shell, so /data cannot be created there. Build it
# here with group 0 and g+rwX rather than a fixed uid: Docker's uid 65532,
# rootless Podman's mapped subuid, and arbitrary-uid runtimes all keep gid 0,
# so every one of them can write to a volume seeded from this directory.
#
# The mode is set on the COPY, not here: a chmod in this stage does not survive
# the copy, which silently yields 0755 and breaks the group-write half of the
# scheme. Owner-write still works, so the mistake passes an ordinary test.
RUN mkdir -p /out/data

# --- debug: same image with a shell --------------------------------------
#
# Built only on demand (--target debug). Exists so `exec` is possible when
# something needs poking; behaviour otherwise matches runtime.
FROM alpine:3.20 AS debug
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/sluice /sluice
COPY --from=builder --chown=65532:0 --chmod=0775 /out/data /data
COPY deploy/config.container.yaml /etc/sluice/config.yaml
USER 65532:0
EXPOSE 8080 8443
# The runtime image has no curl or wget, so the gateway probes itself. See
# cmd/gateway/healthcheck.go.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/sluice", "-config", "/etc/sluice/config.yaml", "-healthcheck"]
ENTRYPOINT ["/sluice"]
CMD ["-config", "/etc/sluice/config.yaml"]

# --- runtime: the default target -----------------------------------------
#
# distroless/static ships a CA bundle (needed: the gateway fetches upstream over
# HTTPS) and tzdata, runs as uid 65532, and contains no shell or package
# manager. Must remain the last stage so a plain `docker build` selects it.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=builder /out/sluice /sluice
COPY --from=builder --chown=65532:0 --chmod=0775 /out/data /data
COPY deploy/config.container.yaml /etc/sluice/config.yaml
EXPOSE 8080 8443
# The runtime image has no curl or wget, so the gateway probes itself. See
# cmd/gateway/healthcheck.go.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/sluice", "-config", "/etc/sluice/config.yaml", "-healthcheck"]
ENTRYPOINT ["/sluice"]
CMD ["-config", "/etc/sluice/config.yaml"]
