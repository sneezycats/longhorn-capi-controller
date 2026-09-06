# syntax=docker/dockerfile:1
# Multi-stage build for longhorn-capi-controller.
# Produces a minimal distroless image containing only the static binary.

ARG GO_VERSION=1.27

FROM golang:${GO_VERSION} AS builder
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
COPY third_party/ third_party/
RUN go mod download

# Copy source.
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
