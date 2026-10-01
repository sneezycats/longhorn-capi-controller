# Validation results — branch `adversarial-review-2026-09-30` (2026-10-01)

- **Lab:** workload cluster `lhcc-roll1` (RKE2 3×CP + 3×workers on Harvester 1.7.1, Longhorn 1.12.1), CAPI management cluster = the Rancher HA cluster; controller in ns `longhorn-capi-system` with `--eviction-timeout=30m --poll-interval=15s`.
- **Companions:** `docs/adversarial-review-2026-09-30.md` (findings + verdicts) · `docs/test-plan-2026-09-30.md` (procedure, ground rules, STOP list).
- **Method:** the plan was executed by an operator agent; all evidence saved outside the repo (`~/scratch/…` on the build host); nothing merged, no PR.

## Round 1 — validation of the original review fixes

Image: `ghcr.io/sneezycats/longhorn-capi-controller:adversarial-review-2026-09-30`.

| Stage/Scenario | Result | Notes |
|---|---|---|
| Stage 0 (gofmt/vet/test/vuln) | PASS | 11 tests (first-ever run); `govulncheck` 0 reachable. Deviation: build host has no Go toolchain — ran containerized `golang:1.27`, repo read-only |
| S1 happy path (`db5jj`) | PASS | hook → eviction → early release in ~2s; replacement `q2mdh` Ready; 4/4 volumes healthy; md5s OK; 0 timeouts/errors |
| **S2 last-live-replica (`5jd9l`, CRIT-1)** | **FAIL — STOP** | see below |
| S3–S8 | not executed | stopped per ground rules |

**S2 failure (CRIT-1a — the validation finding).** Timeline (controller log ⇆ Longhorn events/manager logs):

| t (Z) | actor | event |
|---|---|---|
| 16:42:26 | session | Machine `5jd9l` deleted → controller starts eviction |
| 16:42:27 | controller | correct refusal ×2: `Refusing to delete a volume's last live replica — leaving it for Longhorn's own eviction/rebuild` (`…-r-ce708889`) — survivor count was 0 |
| 16:42:31 | Longhorn | eviction replenishment created replacement `…-r-ae8d8069` on `q2mdh`; process `running` at **16:42:31.9 — zero bytes synced** (source holds ~169MB) |
| 16:42:42 | controller | next 15s poll: the empty replacement counted as a live copy (`isReplicaRunning` = process up, not failed) → `Deleting bad replica to force a clean rebuild` → deleted the migration **source**, the same second the engine began rebuilding from it |
| 16:42:58 | controller | `Early release: … volumes degraded-but-safe …` — the release gate shares the predicate, and had a want=1 degeneracy (`want-1 == 0`) under which zero surviving copies passed as safe |
| 16:43:21 | Longhorn | `FailedRebuilding … connection refused` → "failed to rebuild too many times" → empty replacement cleaned up → volume **FAULTED / detached / 0 replicas**; probe read = `Input/output error` → data lost |

Not the documented backstop case: no `LonghornEvictionTimeout` event, viable rebuild targets existed, release at +32s vs the 30m timeout.

## Fix (CRIT-1a)

Survivor counting — in both `cleanupBadReplicas` and `volumeReplicaState` (the release gate) — now requires the replica to actually **hold the volume's data**: `Spec.HealthyAt` set. Longhorn clears HealthyAt before any rebuild and sets it when the replica goes read/write, so a mid-migration replacement never qualifies and the refusal is sticky until the migration truly completes. The release gate additionally requires `max(want-1, 1)` data-holding replicas (closing the want=1 degeneracy). Five regression unit tests added. Commits: `4487fdf` (fix + tests) → `c5550fe` (gofmt) → `df708b2`/`c8edc06` (docs).

## Round 2 — retest of the fixed branch

Image: `ghcr.io/sneezycats/longhorn-capi-controller:adversarial-review-2026-09-30-s2fix1` (manifest `sha256:2648a896…`). Cluster used as-is; the faulted marker from round 1 had been deleted and its single-replica marker pair re-staged fresh. Two consecutive node-replacement hops — the README rolling-hop matrix.

| Stage/Scenario | Result | Key evidence |
|---|---|---|
| Stage 0 | PASS | 16/16 tests (incl. the 5 new regression tests), vet, gofmt, `govulncheck` 0 reachable |
| S1 (`tzvhq` → `h8srv`) | PASS | hook+eviction 17:46:10; doomed-replica deletion with data-holding survivors; early release 17:46:11; volumes rebuilt 3/3 healthy; md5s OK + `EXISTING-DATA` continuity; Node GC 17:48:24; 0 timeouts |
| **S2 (`6h4j8` — rep1's only replica)** | **PASS** | refusal logged **×4** (17:54:59 ×2, :55:14, :55:30) naming `r-5347db88`; the `Deleted bad replicas` removal lists contain only the 3 multi-replica replicas — the rep1 source appears in **0** of them; Longhorn `Rebuilt` 17:55:30 (replacement `r-e56fb01d`) → source removed :55:40 → **early release :55:45, only after the replacement held data** → volume re-attached to `h8srv` :55:55; writer md5s OK + `EXISTING-DATA`; rep1 healthy |
| S5 EvictStuckPods | PASS | both rounds: force-delete lists = Longhorn-PVC pods only; the non-Longhorn (local-path) pod untouched |
| S6 RBAC can-i | PASS | list secrets no · watch secrets no · get kubeconfig-secret yes · update machines no · patch machines yes |
| S7 Node GC | PASS | tzvhq CR removed by the controller's GC (event `LonghornNodeGC`); 6h4j8 CR also gone without manual action (removal not event-attributed) |
| S8 build hygiene | PASS | digest-pinned base images built/pushed; pod liveness/readiness through both hops |
| S3/S4 | review-only | per plan — not deterministically stageable in a single-cluster lab |

Zero `LonghornEvictionTimeout` events across the round. Final state: 6/6 nodes Ready; all marker volumes healthy; `lhcc-check.sh verify` = ALL MARKERS OK.

## Effect on the review's verdicts

- **CRIT-1 is amended, not overturned** (via CRIT-1a): the finding was real; the first fix's survivor predicate was too weak; the HealthyAt predicate passes S2 end-to-end.
- **CRIT-2 and CRIT-3 are unchanged**; their validation remains review-only (S3/S4).
- Positively exercised by the runs: CORR-1/2 (force-delete scope), CORR-3 (frozen diskStatus → Replica-CR cross-check), CORR-4 (Node GC), SEC-1 (0 reachable), SEC-2/SEC-5 (zero Forbidden; least-privilege `can-i`).
- Verdict stays **Changes Requested**; nothing merged, no PR — maintainer review is the next step.

## Artifacts

- Evidence (round 1): `build:~/scratch/lhcc-review-test-2026-10-01/` — `stage0-static.txt`, `s1-*`, `s2-run.txt`, `s2-verify.txt`, `s2-controller-full.log`.
- Evidence (round 2): `build:~/scratch/lhcc-review-retest-2026-10-01/` — `s1-run.txt`, `s1-verify.txt`, `s2-run.txt`, `s2-verify.txt`, `s2fix1-controller-full.log`.
- Operator runbook with as-built details: lab-docs repo `docs/longhorn/lhcc-review-branch-validation-runbook.md` (§Test-execution log, §Retest round).
- Branch tip at writing: `c8edc06` (GitHub + Forgejo in sync). Old image tag `adversarial-review-2026-09-30` remains published pending maintainer retirement.
