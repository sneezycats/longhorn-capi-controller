# Adversarial code review — 2026-09-30

- **Reviewed:** `main @ 8c39a24` (state published to GitHub 2026-09-29)
- **Fixes:** branch `adversarial-review-2026-09-30` (commit map at the end)
- **Method:** static review only — no tests executed, no code run. Tooling: `govulncheck` (symbol-level reachability), `go vet` (type-check), `gofmt`, live container-registry digest resolution, secrets/credential sweep of all tracked files.
- **Validation:** `docs/test-plan-2026-09-30.md`

Provenance note: at review time the GitHub web UI intermittently displayed the repository as empty; the GitHub API confirmed content (pushed 2026-09-29T15:10:03Z, default branch `main`, 120 KB). The reviewed clone was in sync with `origin/main`.

## Verdict

**Changes Requested.** 2 critical data-loss paths, 3 reachable published vulnerabilities, and several security/convention findings. All proposed fixes are on the branch; none are merged. Tests of the changes are deliberately deferred and are specified separately.

**Update (2026-10-01):** executing the validation plan on lab cluster `lhcc-roll1` found **CRIT-1a** — a data-loss hole in the CRIT-1 fix itself (see below). Fixed on the branch with regression tests; **revalidated 2026-10-01 — S1/S2/S5–S8 PASS, zero timeout events** (runbook: lab-docs `docs/longhorn/lhcc-review-branch-validation-runbook.md` §Retest round).

---

## 🔴 Critical — data-loss paths in replica cleanup

### CRIT-1. `cleanupBadReplicas` could delete a volume's last live replica

`internal/controller/longhornmachineeviction_controller.go` (main, ~:433-444) deleted any replica on a "doomed" node (the departing node, or any node with a deleting Machine) with **no check on what survives**. A single-replica volume (Harvester's default storage class is replica-1) whose only replica sits on the departing node gets its Replica CR deleted → **instant, unrecoverable data loss** — precisely the case where Longhorn's own eviction would have migrated the replica safely given time.

**Fix (75d3340):** two-pass classification — count each volume's running replicas on non-deleting nodes first; refuse to delete a candidate that would leave its volume with zero running replicas ("last live replica" protection). Replicas whose Volume CR no longer exists are garbage and exempt.

### CRIT-2. "Gone node" conflated with NotReady/cordoned nodes

Same function, main ~:434: `onGoneNode` was true for any node not in `liveNodes` (scheduling-eligible + Ready) — but this controller **itself** sets `allowScheduling=false` when triggering eviction, and a flapping/NotReady-but-alive node also fails that test. Its stopped replicas were purged on a transient condition, forcing needless rebuild churn or (for replica-1 volumes) loss. The Node GC's documented invariant ("the k8s Node list is the source of truth; NotReady nodes are never touched") was not shared by the eviction path.

**Fix (75d3340):** a node only counts as "gone" when its **k8s Node object is absent** from the workload cluster — the same source of truth as the Node GC.

### CRIT-3. Cross-cluster node-name collision

`deletingNodeNames` (main ~:385-401) listed **all** Machines in the management cluster. Node names are hostnames and are **not unique across workload clusters**. A Machine being deleted in cluster A marked cluster B's same-named healthy node as doomed → `cleanupBadReplicas` would force-delete its replicas. For a product whose premise is one management cluster watching many workload clusters, this was the sharpest correctness bug in the repo.

**Fix (75d3340):** `deletingNodeNames` is scoped to the Machine's own cluster (same namespace + `spec.clusterName`). Within the cluster it still covers *any* doomed node, not just the reconciled Machine.

### CRIT-1a. The CRIT-1 guard counted unsynced replacement replicas (validation finding, 2026-10-01)

Found by executing S2 of the validation plan on lab cluster `lhcc-roll1` (Longhorn 1.12.1): the last-live-replica guard's survivor count treated **any running replica** as a surviving copy. Longhorn's own eviction replenishment creates a replacement replica within seconds — its process reports `running` immediately, but it holds **no data** until its rebuild completes (169MB in the observed run). One 15s poll later the guard saw "a live copy exists", dropped its refusal, and deleted the migration **source** — the only replica holding data — in the same second the engine began rebuilding from it. The in-flight rebuild died (`connection refused`), the empty replacement was cleaned up, and the single-replica volume ended FAULTED with zero replicas and unrecoverable data about a minute after trigger (nowhere near the 30m backstop; viable rebuild targets existed). The early-release gate (`volumeReplicaState`) shared the same predicate and released the hook as "degraded-but-safe" during the migration; it also had a want=1 degeneracy (`want-1 == 0`) under which a volume with **zero** surviving copies passed as safe.

**Fix:** survivor counting — in both the cleanup guard and the release gate — now requires the replica to actually **hold the volume's data**: `Spec.HealthyAt` set. Per the Longhorn API contract, HealthyAt is cleared before any rebuild and set when the replica goes read/write, so a mid-migration replacement never qualifies and the refusal is sticky until the migration truly completes. The release gate additionally requires `max(want-1, 1)` data-holding replicas, closing the want=1 degeneracy. Regression tests added: refusal while the replacement is mid-rebuild; deletion allowed once the replacement holds data; S1 semantics (multi-replica cleanup proceeds); early-release hold for a want=1 volume mid-migration; refusal when the only survivor on a multi-replica volume is mid-rebuild.

**Effect on the review's verdicts (validation addendum, 2026-10-01):** CRIT-1a **amends** the CRIT-1 fix — the underlying finding (deleting a volume's last live replica) was real, but the first fix's survivor predicate was too weak; with the HealthyAt predicate the S2 scenario passes end-to-end on `lhcc-roll1` (refusal sticky through Longhorn's migration, data intact, hook released only after the replacement holds data). **CRIT-2 and CRIT-3 are unchanged** — their deterministic validation paths (S3/S4) remain review-only per the test plan, as a single-cluster lab cannot stage them. The runs did positively exercise CORR-3 (frozen diskStatus → Replica-CR cross-check), CORR-1/2 (EvictStuckPods force-delete lists contained only Longhorn-PVC pods), CORR-4/Node GC (orphaned LH Node CR removed), SEC-2/SEC-5 (zero Forbidden; `can-i` matches least-privilege), and SEC-1 (govulncheck 0 reachable). Verdict stays **Changes Requested**; nothing merged, no PR.

---

## ⚠️ Security / published vulnerabilities

### SEC-1. Reachable CVEs in dependencies (govulncheck, symbol-level)

- **GO-2026-4918** — HTTP/2 transport infinite loop (`golang.org/x/net` < v0.53.0); reached via every workload-cluster client call.
- **GO-2026-5026** — ASCII-only punycode labels not rejected (`golang.org/x/net/idna` < v0.55.0); reached via client List.
- **GO-2026-5970** — infinite loop on invalid input (`golang.org/x/text` < v0.39.0); reached via client List.

**Fix (9dacfe2):** `golang.org/x/net` v0.38.0 → v0.55.0, `golang.org/x/text` v0.23.0 → v0.39.0 (plus transitive x/sync, x/sys, x/term). After the bump: **0 reachable** (1 module-level unreached finding remains, down from 9 — a full k8s 0.32.x bump is the eventual fix and was deliberately not attempted untested).

### SEC-2. Kubeconfig Secrets read through the cached client

`buildWorkloadClusterClient` (main ~:810) read the `<cluster>-kubeconfig` Secret with the manager's **cached** client. The first `Get` on a Secret lazily starts a Secret informer that **lists and watches every Secret in the management cluster** — pulling every workload cluster's admin credential into controller memory, and it is why the RBAC "needed" cluster-wide `secrets: list, watch`.

**Fix (31405e8):** Secret reads go through the manager's uncached `APIReader` (`mgr.GetAPIReader()`); RBAC drops to `secrets: get` only.

### SEC-3. Lab-specific identity shipped in public RBAC

`config/rbac/workload_role.yaml` (main :62) bound the ClusterRole to a specific Rancher-issued user id (`u-zssbnetv6a`) plus a ServiceAccount subject that does not exist in workload clusters. **Fix (31405e8):** replaced with an explicit `REPLACE_WITH_KUBECONFIG_IDENTITY` placeholder + instructions; the stale, incomplete "namespaced variant" comment block (it omitted the cluster-scoped pod/node rules the feature actually needs) was removed.

### SEC-4. Supply chain: floating base image tags

`Dockerfile` used floating tags (`golang:1.27`, `gcr.io/distroless/static:nonroot`). **Fix (af81bbe):** digest-pinned multi-arch indexes (digests resolved live from the registries at review time).

### SEC-5. RBAC least-privilege

- management role: `machines` drops `update` (only PATCH is used) and the unused `machines/status` rule; `secrets` drops `list, watch` (see SEC-2).
- workload role: drops unused `update` on `longhorn.io/nodes` and core `nodes`.
- kubebuilder markers kept in sync with the manifests.

---

## ⚠️ Correctness (lower severity)

### CORR-1. EvictStuckPods evicted on unverified state and beyond documented scope

Main ~:766-798: `podHasLonghornPVC` matched **any** PVC, and `podVolumesFaulted` could never return an error (every lookup failure was swallowed as "not faulted") — making the loop's skip-on-error guard dead code. Any pod with any PVC on the departing node was force-deleted on unverified state, including pods not backed by Longhorn at all — contradicting the flag help text and README, which promise Longhorn PVCs only.

**Fix (e6eef67):** positive Longhorn match required (PVC must resolve to a Longhorn Volume CR); transient lookup errors skip the pod ("never evict on unverified state"); the unit test now asserts non-Longhorn-PVC pods are **not** deleted.

### CORR-2. "Force-delete" wasn't

Main :709 used a plain `Delete`. On a node whose kubelet is gone, that leaves the pod `Terminating` forever — the exact stuck state the feature exists to clear. **Fix (e6eef67):** `client.GracePeriodSeconds(0)` — an actual force-delete. Cordon-patch failures are now logged instead of silently ignored.

### CORR-3. Vacuous-truth drain check

`isEvictionComplete` (main :343-345) treated a nil/empty `diskStatus` map as "eviction complete" — but an empty map also means the node never reported anything (fresh CR, or longhorn-manager died before reporting), which is UNKNOWN, not complete. **Fix (31405e8):** returns false, so the Replica-CR cross-check decides.

### CORR-4. GC client-cache hygiene

`LonghornNodeGCReconciler` cached workload clients forever: kubeconfig rotation or token expiry (401/403) poisoned a cluster's client permanently. **Fix (31405e8):** auth failures drop the cached client so the next pass re-reads the Secret. The periodic sweep also discarded reconcile errors silently — now logged.

---

## 💡 Conventions (Go/k8s community standards)

Fixed inline:

- `var _ = metav1.Now` import-retention hack (with a "metatime" typo in its comment) — removed (31405e8).
- Unused `deletingNodes` parameter on `nodeReplicasDrained` — removed (31405e8).
- README claimed "Go 1.22+ to build" while `go.mod` requires `go 1.27.0` — corrected (af81bbe).
- No `Makefile`, `SECURITY.md`, or `.dockerignore` for a public Go/k8s repo — added (af81bbe). CI recommended but not added (workflow design is a maintainer decision).

Noted, deliberately left (behavior changes / cosmetic):

- `removeHook`: dead `Annotations == nil` guard after `latest.Annotations[HookAnnotation]` was already read — harmless.
- `envBool` accepts a non-`strconv.ParseBool` set (`yes/no/on/off`) — a behavior change if "fixed"; left as-is.
- `fmt.Fprintf(os.Stderr, ...)` warnings before the logger is initialized — acceptable pattern.

---

## Deliberately NOT changed (flagged for maintainer)

1. **`volumeReplicaState` gates on every volume in the Longhorn namespace, not just eviction-affected ones.** The README said "affected volumes"; the code says "all volumes". Docs were corrected instead of the code (af81bbe): narrowing the gate to "has doomed replicas" breaks `--early-release=false` strict mode, because cleanup deletes the doomed Replica CRs first, after which those volumes look unaffected and the gate vacuously passes at want−1. The proper fix is snapshotting the affected set onto the Machine at hook registration — a design change that should not land untested.
2. **Test coverage gap:** `cleanupBadReplicas` — the most dangerous function in the repo — had zero unit tests before this branch. The CRIT-1a fix (below) adds regression tests for the last-replica refusal paths; gone-vs-NotReady classification and per-cluster deleting-node scoping remain review-only (S3/S4).
3. **Version skew:** vendored Longhorn types pinned at longhorn-manager v1.8.1 while the README validates against Longhorn 1.12.x. Fine today (v1beta2 is the stable API surface); worth a periodic re-vendor.

---

## Verification performed (static only)

| Check | Result |
|---|---|
| `gofmt -l cmd internal` | clean |
| `go vet ./...` (full type-check incl. test files) | exit 0 |
| `go test` | **NOT RUN** (deferred by request) |
| `govulncheck ./...` on `main` | 3 reachable, 9 module-level |
| `govulncheck ./...` on branch | **0 reachable**, 1 module-level unreached |
| Secrets/credential sweep of tracked files | clean (`prompt.md`/`notes.md` correctly git-ignored) |
| Base-image digests | resolved live from Docker Hub / gcr.io registries |

## Commit map

| Commit | Fixes |
|---|---|
| `9dacfe2` | SEC-1 (dependency CVE bumps) |
| `75d3340` | CRIT-1, CRIT-2, CRIT-3 (replica-cleanup safety) |
| `e6eef67` | CORR-1, CORR-2 (EvictStuckPods) |
| `31405e8` | SEC-2, SEC-3, SEC-5, CORR-3, CORR-4, convention fixes |
| `af81bbe` | SEC-4, build/docs hygiene, README accuracy |
| `9104955` | review record + test plan |
| (this commit) | CRIT-1a: data-holding survivor predicate (`HealthyAt`), want=1 release-gate fix, regression tests |

**Status:** branch pushed, not merged, no PR. Validation of these changes is specified in `docs/test-plan-2026-09-30.md` and must run before merge.
