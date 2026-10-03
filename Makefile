# Minimal kubebuilder-style targets. RBAC/manifests are maintained by hand
# (no controller-gen), so there is no `manifests` target.

BINARY ?= bin/manager
IMAGE  ?= ghcr.io/sneezycats/longhorn-capi-controller:dev

.PHONY: all build test fmt vet vuln docker-build docker-check gate clean

all: build

build:
	mkdir -p bin
	go build -o $(BINARY) ./cmd

test:
	go test ./...

fmt:
	gofmt -l -w cmd internal

vet:
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

docker-build:
	docker build -t $(IMAGE) .

docker-check:
	docker build --target=check .

# Full deterministic gate (in-container tests + govulncheck on the binary);
# PUBLISH=1 IMAGE=... make gate  pushes and re-verifies from the registry.
gate:
	IMAGE=$(IMAGE) PUBLISH=$(PUBLISH) bash scripts/check.sh

clean:
	rm -rf bin
