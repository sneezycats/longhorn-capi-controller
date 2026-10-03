#!/usr/bin/env bash
# Deterministic check + optional publish for the lhcc image.
#
# Gates, in order — the script aborts at the first failure:
#   1. docker build --target=check .   (go.sum integrity + unit tests, in the
#                                      pinned golang stage — no host Go needed)
#   2. docker build -t "$IMAGE" .      (full image; base digests are pinned)
#   3. govulncheck -mode=binary        (reachability-gated Go vuln scan of the
#                                      actual /manager artifact, symbol level)
#   4. docker scout cves               (informational when the plugin exists;
#                                      Docker Hub Scout is the cloud twin)
#
# Publish only happens explicitly:  PUBLISH=1 scripts/check.sh
# Successful push is RE-verified by pulling the image back and re-running
# govulncheck on the pulled binary.
#
# Depends on: docker (+buildx), govulncheck. Nothing else. No lab state, no
# secrets in this file — auth comes from the host's docker login / gh state.
#
# Run on the amd64 build host so the analyzed artifact is the exact published
# architecture. On arm64 dev machines the same gates run, but the binary is
# arm64 — rescan the amd64 image before publishing.

set -euo pipefail

IMAGE="${IMAGE:-ghcr.io/sneezycats/longhorn-capi-controller:dev}"
GOVULNCHECK="${GOVULNCHECK:-$(command -v govulncheck || true)}"
DO_PUBLISH="${PUBLISH:-0}"
DO_SCAN_BIN="${SCAN_BIN:-1}"

[[ -x "$GOVULNCHECK" ]] || { echo "check.sh: govulncheck not found" >&2; exit 5; }

echo "==> [1/4] in-container gates (go.mod/go.sum verify + unit tests)"
docker build --target=check .

echo "==> [2/4] image build: $IMAGE"
docker build -t "$IMAGE" .

tmp="$(mktemp -d)"
tmphold=""
cleanup() { rm -rf "$tmp" "$tmphold"; [[ -z "${cid:-}" ]] || docker rm -f "$cid" >/dev/null 2>&1; }
trap cleanup EXIT

if [[ "$DO_SCAN_BIN" == "1" ]]; then
  echo "==> [3/4] govulncheck -mode=binary on /manager (reachability gate)"
  cid="$(docker create "$IMAGE")"
  docker cp "$cid:/manager" "$tmp/manager" >/dev/null
  docker rm -f "$cid" >/dev/null; cid=""
  "$GOVULNCHECK" -mode=binary "$tmp/manager"
  echo "    no reachable vulnerabilities — gate passed"
else
  echo "==> [3/4] SKIPPED (SCAN_BIN=0)"
fi

if command -v docker >/dev/null 2>&1 && docker scout version >/dev/null 2>&1; then
  echo "==> [4/4] docker scout cves (informational; not a gate)"
  docker scout cves --only-severity critical,high "$IMAGE" || \
    { echo "    scout reported findings above — inspect before publishing" >&2; }
else
  echo "==> [4/4] docker scout plugin not installed — skipping (Docker Hub Scout covers this)"
fi

if [[ "$DO_PUBLISH" != "1" ]]; then
  echo
  echo "All gates passed. Artifact: $IMAGE"
  echo "Publish with: PUBLISH=1 IMAGE=$IMAGE $0"
  exit 0
fi

echo "==> publish: docker push $IMAGE"
docker push "$IMAGE"

echo "==> re-verify: drop the local copy, pull the REGISTRY artifact back, rescan"
docker image rm -f "$IMAGE" >/dev/null 2>&1 || true
docker pull "$IMAGE" >/dev/null
tmphold="$(mktemp -d)"
cid="$(docker create "$IMAGE")"
docker cp "$cid:/manager" "$tmphold/manager" >/dev/null
docker rm -f "$cid" >/dev/null; cid=""
"$GOVULNCHECK" -mode=binary "$tmphold/manager"
echo "==> published and re-verified clean from the registry: $IMAGE"