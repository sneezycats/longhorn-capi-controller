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

## Round 3 — upgrade cycle 6.0→6.1 (STOPPED at a new finding; 6.2 not attempted)

**Trigger:** one-line image hop in the `lhcc-roll-test` Forgejo repo (`cluster-templates/chart/values.yaml` `imageName: sl-micro-longhorn-6.0-v4g` → `6.1-v4g`, commit `4187dda`) → Fleet → Rancher rolled both pools via machine-set-driven machine replacements (new pools: `qk6vv` CPs, `2qtrv` workers). 19:53–20:36Z.

**What held — the controller behaved correctly through every hop:**
- Each machine deletion: hook registration → LH eviction trigger → correct refusal for rep1's single replica (`h8srv` hop: ×4 for `…e56fb01d`; `jnt4m` hop: ×7+ for `…516bd1e3`) → doomed multi-replica replicas deleted only with data-holding survivors present → early release only when degraded-safe → EvictStuckPods lists correct → Node GC. No crash-loops, no timeout events, pod 0 restarts.
- The 3 multi-replica volumes: healthy 3/3 after the full roll on the new workers; data continuous end-to-end (counters 226→648, md5s OK, `EXISTING-DATA` across every hop).

**What failed — UPD-1 (1-replica volume lost in the roll flow):**
The rep1 volume's only data-holding replica died **with the jnt4m VM**: machine deletion 20:10:00 → the machine **drain evicted the instance manager** (~20:10:1x; replica `mode ERR` at 20:10:16-18) → the **VM was shut down and deleted ~20:10:10–20:11** (harvester events: "Stopped the virtual machine by deleting the virtual machine instance") → volume `faulted/detached`; LH auto-salvage loops "Bringing up 0 replicas" (disk gone) → unrecoverable. The controller's refusals held to the very end — the controller deleted nothing; the teardown killed the last copy.

**Mechanism (why the earlier hops survived):** the protective window in every prior successful hop came from the volume's **attachment to the departing node blocking the machine drain** (CAPI `WaitingForVolumeDetach`), not from the pre-terminate hook. In this hop the volume was attached to `gwt8s` (the writer had already moved to the new worker) with the replica on `jnt4m` — no attachment on the departing node → the drain unblocked instantly → the VM was deleted ~10–16s after deletion began, **while the hook was held** and LH's eviction had produced no migration (no replacement replica CR was ever created on the new workers). The hook (standard `pre-terminate.delete.hook.machine.cluster.x-k8s.io/…`) gated only late machine-deletion steps: the machine CR lingered in `Deleting` for 25+ min with the hook held, but the VM teardown was not gated by it in this path.

**Implication:** for 1-replica volumes the controller + LH eviction cannot reliably protect the last replica in the machine-set roll flow — LH's eviction needs a window that only exists when the volume is attached to the departing node, which is uncommon mid-roll. Design directions for the maintainer: (a) controller-side — at hook registration, for each volume whose only data-holding replica sits on the departing node, ensure an LH attachment of that volume to the departing node until LH's eviction relocates the replica (recreates the drain blocker deterministically); (b) verify upstream whether Rancher 2.14/CAPRKE2 honors pre-terminate hooks before drain+infra teardown in machine-set-driven deletions; (c) operational — single-replica storage classes must not be trusted across rolling upgrades (the test SC is single-replica by design).

**Interventions (documented, environment-level):** eviction-timeout 30m→90m live deployment edit at 20:30 (insurance against the backstop data-loss; superseded by the finding); released the dead machine's hook at 20:36 (machine finalized, node + LH CR cleaned); rep1 PVC deleted (faulted volume CR + PV removed).

**State after:** 6/6 nodes on SL Micro 6.1 (qk6vv CPs + 2qtrv workers); 3 multi-replica volumes healthy; local-store intact; controller 1/1 Running (args `--eviction-timeout=90m`). 6.2 hop NOT attempted.

## Artifacts

- Evidence (round 1): `build:~/scratch/lhcc-review-test-2026-10-01/` — `stage0-static.txt`, `s1-*`, `s2-run.txt`, `s2-verify.txt`, `s2-controller-full.log`.
- Evidence (round 2): `build:~/scratch/lhcc-review-retest-2026-10-01/` — `s1-run.txt`, `s1-verify.txt`, `s2-run.txt`, `s2-verify.txt`, `s2fix1-controller-full.log`.
- Evidence (round 3): `build:~/scratch/lhcc-upgrade-cycle-2026-10-01/` — `roll-6.1-part1/2/3.txt`, `cycle-controller-full.log`, `roll-6.1-rep1-watch.txt`, `interventions.txt`.
- Evidence (round 4, same dir): `retest-a-7ftj5.txt` (controlled UPD-1-geometry save), `roll-6.2-part1/2/3.txt` (full 6.1→6.2 roll: 2 saves + 1 teardown-beat-rebuild loss).
- Operator runbook with as-built details: lab-docs repo `docs/longhorn/lhcc-review-branch-validation-runbook.md` (§Test-execution log, §Retest round).
- Branch tip at writing: `c8edc06` (GitHub + Forgejo in sync). Old image tag `adversarial-review-2026-09-30` remains published pending maintainer retirement.

## Round 4 — CRIT-1b fix implemented and rolled (2026-10-01, 21:50–22:36 UTC)

**Fix deployed:** branch commit `041f0a2` (+test commits, tip `432a68f`) implements relocation:
- `relocateLastReplicas` — at hook registration, for every volume whose ONLY data-holding replicas sit on nodes CAPI is deleting: record the original `spec.numberOfReplicas` on the Machine (`longhorn-capi.io/replica-scale-orig` annotation) and raise the count by the number of doomed data-holding replicas → Longhorn's standard scheduling rebuilds a replacement onto a surviving node immediately. Idempotent via the annotation.
- `restoreReplicaWants` — restores raised counts from every release path (timeout / no-NodeRef / LH-node-gone / early / complete / post-release 0.8 lingering).
- `healFailedSourceReplicas` — uncordons a still-existing node whose data-bearing replica failed mid-relocation (LH can recreate the IM and salvage from the intact disk).
- RBAC: workload ClusterRole gains `volumes patch`. 13 new unit tests; suite 28/28, gofmt/vet clean.
- Image: `ghcr.io/sneezycats/longhorn-capi-controller:adversarial-review-2026-09-30-c1bfix1` (digest `sha256:6058f032…`). NOTE: the lab controller identity is bound to `cluster-owner` (the least-privilege ClusterRole exists but the ClusterRoleBinding was never repointed) — the relocation patches exercised admin perms; RBAC least-privilege remains an open maintainer item.

**Retest A — controlled UPD-1 geometry (manual machine delete, 21:58):** SAVE. Hook+eviction 21:58:45, refusal held, relocation raised want 1→2 at 21:58:46, LH scheduled the replacement 21:59:01, rebuild DONE 21:59:06 (21s), doomed source died with the VM at 21:59:17 — no matter. Early release + want restored to 1 at 21:59:32; machine finalized; volume healthy; writer IO uninterrupted end-to-end (its attachment never moved — exactly the geometry that lost in round 3).

**Full 6.1→6.2 roll (commit `e21e797`, 22:08–22:32):** the machine-set replaced BOTH pools (workers 2qtrv→ldn5c, CPs 56l7z→new). 5 hops observed:
- CP hops: hook → immediate release (no Longhorn node CR). Clean.
- Worker hop 22:13: multi-replica volumes degraded-but-safe → fast release. Clean.
- Worker hop 22:18: relocation SAVE — Refusing→Relocated 22:18:40 → refusals held → doomed replica deleted + early release + count restored 22:19:26. Volumes back to healthy 22:20:58.
- **Worker hop 22:24 (last old worker `j29hk`, single-replica volume): LOSS by ~5s.** Fix fired correctly (Refusing + Relocated at 22:24:30, want 1→2), but this VM's teardown was unusually fast — VMI deleted 22:24:51, **21s after the relocation** (prior teardowns: 32s, 46s, ~104s) — the rebuild (~25–30s incl. scheduling) was still in flight when the source disk died. Volume faulted/detached; salvage impossible (kubelet died with the VM; the heal's precondition — a live guest — was already gone). Hook held until manual release 22:31:24; rep1 fixture PVC deleted.

**Cycle final state:** 6/6 machines+nodes on SLE Micro 16.0 (=6.2 image), all Ready; 3 multi-replica volumes healthy/attached with **md5 probe + 64MB probe clean and counters advancing (seq 22–31) across THREE pool generations** (sb2fw 6.0 → 2qtrv 6.1 → ldn5c 6.2); controller 1/1 Running throughout, zero timeouts, zero crash-restarts; no FAULTED on any scenario-predicted-healthy volume.

**Verdict (updated from round 3's design directions):** direction (a) was implemented as relocation — it converts the 1-replica roll-flow window from "LH eviction must fire (rare)" to "rebuild must beat the VM teardown (~21–46s observed, 3 saves / 1 loss)". **The race cannot be closed from inside the workload cluster**: the VM deletion is hook-blind, the disk dies with the VM, and teardown duration is guest-dependent (idle guests die faster). Direction (c) is now a hard recommendation: single-replica storage classes must never hold data that must survive rolling upgrades; the test SC is single-replica precisely to exercise this edge. Residual 1-replica risk should be surfaced in the README as a known limitation.

## Round 5 — roll2 full cycle on the critical-review fleet config (2026-10-02, 01:41–02:24 UTC)

**Environment (rebuilt from scratch):** lhcc-roll1 torn down; lhcc-roll2 (c-m-pbf2xftn, 3CP+3W, RKE2 v1.33.13+rke2r1) built at 6.0-v4g from `cwillis/rancher-fleet-example` @ `critical-review-2026-10-01` via instance repo `cwillis/lhcc-roll2`. The fleet config carries `drainBeforeDelete: true` + `drainBeforeDeleteTimeout: 3600s` on the WORKERS pool only (CP renders false — Rancher convention). Build-time finding: Rancher's provisioning webhook unmarshals `drainBeforeDeleteTimeout` as a Go duration — rendering the empty string fails CREATE with `invalid duration ""`; the template now omits the key when unset (fix on the branch). Controller: c1bfix1 on the mgmt cluster (`--eviction-timeout=90m`), workload access via the Rancher-proxy ClusterIP kubeconfig secret (`lhcc-roll2-kubeconfig`, works in-cluster without DNS). Markers: 3 multi-replica (branch harness, lh-test-*) + rep1 worst-case staged at deploy time (2Gi, SC longhorn-rep1-test). Baseline 01:40:32.

**Hop 1 (6.0→6.1, 01:41–02:03, ~21 min):** 3 CP hops (hook → fast release, no LH node CR) + 3 worker hops. Two fast releases (multi-replica degraded-but-safe). **Third worker hop = full relocation save:** hook 01:58:52 → Refusing + **Relocated** (want 1→2) same second → refusals held ~15s → Deleted bad + Early release + **Restored original replica count** 01:59:23 (31s end-to-end). Volumes 4×healthy by 02:01. **Zero faulted.**

**Hop 2 (6.1→6.2, 02:03–02:24, ~21 min):** same shape; **relocation save fired again** (02:13:38 → release+restore 02:14:10, 32s) + fast releases; final worker hop fast-path. **Zero faulted.**

**Final verify (02:24):** ALL VOLUMES OK — created-at unchanged from the pre-hop baseline (01:39:54/58), counters 2→80–84 across BOTH hops, `CONTINUITY=EXISTING-DATA` on every writer, all 4 volumes healthy/attached on the crpdq pool, rep1 want restored to 1 (LH trimmed the relocation excess; 1 replica on crpdq-xfl4x). rep1 md5 probes OK (probe.input + 64MB probe). Controller pod 0 restarts, no timeout events, no crash-loops, no checksum mismatches. 6/6 nodes on SLE Micro 16.0 (=6.2 image).

**Assessment:** with `drainBeforeDelete` restoring drain-then-delete sequencing, the hops ran as calm sequential drains (~1–2 min per worker, pods evicted and attachments moved before VM deletion) instead of round-3/4's parallel hook-blind teardowns. The relocation fix fired correctly on both single-replica exposures (2/2 saves) and released cleanly every time. The combined stack (fleet drain flag + relocation + refusal gates) completed 6.0→6.1→6.2 with **no data loss and zero scenario-unpredicted FAULTED volumes**. Note: the branch's multi-replica writers do not create md5 probe files (created-at/counter continuity only); rep1 carries the md5 evidence.

Evidence: `/tmp/lhcc-roll2/hop-6.1-part1/2/3.txt`, `hop-6.2-part1/2/3.txt` (Mac); branch tip at writing: docs commit on `adversarial-review-2026-09-30`.
