# syntax=docker/dockerfile:1
# Multi-stage build for longhorn-capi-controller.
# Produces a minimal distroless image containing only the static binary.
#
# Stages:
#   base    — pinned Go toolchain; dependencies fetched into a cached layer
#   check   — deterministic gates: go.sum vs go.mod integrity + unit tests.
#             Run standalone with `docker build --target=check .`
#             (no host Go toolchain required — the gate runs in-container).
#   builder — cross-compiles the static /manager binary
#   final   — distroless runtime image (default target)
#
# Base images are digest-pinned (multi-arch indexes) so builds are
# reproducible and auditable — bump tag and digest together.

FROM golang:1.27@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS base
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
COPY third_party/ third_party/
RUN go mod download

FROM base AS check
# Gate stage: go.sum must match go.mod (deterministic resolution), then all
# unit tests must pass. Fails the build otherwise.
COPY cmd/ cmd/
COPY internal/ internal/
RUN go mod verify && go test ./...

FROM base AS builder
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /manager ./cmd

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]