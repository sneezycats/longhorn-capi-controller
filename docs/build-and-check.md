# Build, check, and publish

Deterministic pipeline for building, gating, and publishing the controller
image. Everything below needs only **docker** and **govulncheck** on the host —
no Go toolchain, no lab state, no secrets in the repository.

## Why this shape

- Base images are digest-pinned, so identical inputs produce identical
  outputs.
- `go.sum` pins every module hash; `go mod verify` (gate 1) fails the build
  if `go.mod` and `go.sum` ever drift apart — there is no silent
  re-resolution.
- The Go gates run **inside** the pinned `golang` build stage, so the host
  never needs a Go toolchain (the build host intentionally has none).
- `govulncheck -mode=binary` scans the actual `/manager` artifact at symbol
  (reachability) level — a version matched by a scanner but never linked
  into a reachable path is reported, not fatal (it still fails the gate on
  reachable findings, which is the correct bar for a network-facing
  controller holding kubeconfig credentials).

## The gates (scripts/check.sh)

| # | Gate | What it catches | Failure mode |
|---|------|-----------------|--------------|
| 1 | `docker build --target=check .` | `go.sum`/`go.mod` drift; unit-test regressions | build fails |
| 2 | `docker build -t $IMAGE .` | reproducible image (pinned bases) | build fails |
| 3 | `govulncheck -mode=binary $IMAGE:/manager` | reachable known CVEs in the shipped binary (level: symbol) | non-zero exit aborts the script |
| 4 | `docker scout cves` (when installed) | base-image OS-level advisories | informational; Docker Hub Scout is the cloud twin |

## Run

```sh
# gates only (recommended for every change that touches the image):
bash scripts/check.sh
# or: make gate

# gates + publish + registry re-verify (explicit opt-in only):
PUBLISH=1 IMAGE=ghcr.io/sneezycats/longhorn-capi-controller:0.11.0a bash scripts/check.sh
```

Publish deletes the local image, pulls the **registry** copy back, and
re-scans the pulled `/manager` — so the verified artifact is the one actually
served, not a local build.

## Architecture

Run the full path on an **amd64** host (the build host) so the analyzed
binary and the published binary are the same platform. On arm64 dev machines
the gates run fine but produce an arm64 artifact; rescan the amd64 image
before publishing.

## Publish anatomy (per release)

1. Any `go.mod`/`go.sum` change commits with a `fix:` message naming the
   CVE/GO-ID and the fixed version (e.g.
   `fix: bump golang.org/x/net v0.55.0 → v0.56.0 (CVE-2026-46600 / GO-2026-5942)`),
   so the image provenance attestation links to the exact source.
2. Tag the commit, bump `config/manager/kustomization.yaml` `newTag`, and push
   `ghcr.io/sneezycats/longhorn-capi-controller:<tag>` via
   `PUBLISH=1 IMAGE=... scripts/check.sh`.
3. Re-run Docker Hub Scout on the pushed tag (cloud twin of gate 4) — that is
   the same scanner that first caught CVE-2026-46600.