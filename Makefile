# Minimal kubebuilder-style targets. RBAC/manifests are maintained by hand
# (no controller-gen), so there is no `manifests` target.

BINARY ?= bin/manager
IMAGE  ?= ghcr.io/sneezycats/longhorn-capi-controller:dev

.PHONY: all build test fmt vet vuln docker-build clean

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

clean:
	rm -rf bin
