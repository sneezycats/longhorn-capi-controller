# longhorn-capi-controller

A Kubernetes controller that automates **safe Longhorn data handling during Cluster API (CAPI) node
replacement**. It runs in the **CAPI management cluster** (Rancher, Harvester, Cluster API
standalone) and guards the window where a `Machine` is deleted but its Longhorn replicas have not
yet been safely rebuilt elsewhere.

**Why it exists:** when CAPI replaces a worker node (OS image roll, instance type change, fleet
update, manual delete), the Longhorn replicas on that node's disks are destroyed with the VM. If the
replacement logic does not wait for those replicas to be rebuilt on surviving nodes, volumes go
degraded — and if too many nodes are replaced concurrently, data is lost. The official
[Longhorn KB: How to evict node during CAPI node rolling replacement](https://longhorn.io/kb/how-to-evict-node-during-capi-node-rolling-replacement/)
documents the manual procedure. The Longhorn maintainers declined to ship a controller for it
([longhorn/longhorn#12871](https://github.com/longhorn/longhorn/issues/12871): *"we don't implement
such operator, KB is the guide"*) — this repository is that implementation.

> **Status:** running in production-shaped lab environments (Harvester + Rancher + RKE2) through
> ~40 validated node replacements across OS upgrades (SL Micro 6.0→6.1→6.2), 1/3/5/6-worker
> clusters, single-disk and dedicated-data-disk layouts, concurrent deletions, and controller
> restarts — with zero data loss and zero manual intervention. See
> [Validation](#validation) for the test matrix.

---

## How it works

CAPI supports lifecycle hooks on `Machine` deletion. This controller uses the **pre-terminate
hook**: CAPI pauses the deletion after pod drain and waits until every
`pre-terminate.delete.hook.machine.cluster.x-k8s.io/*` annotation is removed from the Machine. The
controller drives the entire safe-eviction sequence between "drain done" and "delete the VM":

```
Machine deletion detected
  └─► register pre-terminate hook (timestamped, survives controller restarts)
       └─► trigger Longhorn eviction on the departing node
            (allowScheduling=false, evictionRequested=true)
            └─► wait for eviction drain
                 ├─ diskStatus empty on the node, OR
                 └─ authoritative cross-check: no RUNNING replica CRs remain on
                    the node (covers the dead-manager frozen-status case)
                      └─► replica-rebuild gate
                           ├─ all affected volumes have their full replica
                           │  count on non-deleting nodes → release, or
                           └─ EARLY RELEASE: every affected volume still has
                              ≥ want−1 replicas on surviving nodes and none is
                              faulted → release now (degraded-but-safe; the
                              rebuild cannot proceed until the departing node
                              leaves the cluster in 1-disk-per-node topologies)
                                └─► remove hook → CAPI deletes the VM
                                     └─► EvictStuckPods: if CAPI then stalls at
                                          its volume-detach stage (a workload pod
                                          still holds the RWO attachment), cordon
                                          the node and delete those pods
                                     └─► Node GC: sweep orphaned
                                          nodes.longhorn.io CRs whose k8s Node
                                          is gone (stuck-finalizer workaround)
```

A backstop timeout (`--eviction-timeout`, default **2h**) releases the hook no matter what, so CAPI
is never blocked forever. All controller state lives on the Machine annotations — the controller is
stateless and safe to restart at any point.

### Additional hardening built in

- **Frozen-status cross-check:** a departing node's longhorn-manager can die mid-eviction, leaving
  its `diskStatus.scheduledReplica` map stale forever. The controller treats the Replica CRs as the
  authoritative source and will not wait on a dead node's self-reported status.
- **Bad-replica cleanup:** a running replica on *any* CAPI-deleting node (including a different
  node than the one being evicted) is deleted to force a clean rebuild — closes the
  "volume looks healthy but a replica lives on a doomed node" trap.
- **Node-CR garbage collection:** Longhorn fails to remove `nodes.longhorn.io` CRs for nodes that
  died before deletion ([longhorn/longhorn#6487](https://github.com/longhorn/longhorn/issues/6487),
  wontfix). The GC removes them, using the k8s Node list as the source of truth: a Longhorn node
  whose k8s Node still exists is **never** touched.

---

## Compatibility

| Component | Tested with |
|---|---|
| CAPI | v1.10.x (`cluster.x-k8s.io/v1beta1` Machines) |
| Longhorn | v1.11.x (v1 data engine) in workload clusters |
| Management cluster | Rancher 2.15 provisioned RKE2 + Harvester node driver |
| Workload clusters | RKE2 on Harvester VMs (SL Micro), single CP + 1–6 workers |
| Go | 1.22+ to build |

Should work with any CAPI provider that produces standard `Machine` CRs with `status.nodeRef` and
per-cluster kubeconfig Secrets (kubeadm, RKE2, Talos providers, etc.). Non-Rancher setups are
untested — reports welcome.

---

## Installation

### Prerequisites

1. A CAPI management cluster with `Machine`/`Cluster` CRDs.
2. Longhorn installed in each workload cluster whose nodes you want protected.
3. Workload cluster kubeconfigs stored as Secrets named `<cluster-name>-kubeconfig`, in the same
   namespace as the Machines, with the raw kubeconfig under the `value` key (the standard CAPI
   convention — CAPI creates these automatically).

### 1. Deploy to the management cluster

```bash
kubectl apply --server-side -k config/manager
```

Or build and push your own image first:

```bash
docker build -t ghcr.io/<you>/longhorn-capi-controller:$(git rev-parse --short HEAD) .
docker push ghcr.io/<you>/longhorn-capi-controller:$(git rev-parse --short HEAD)
# then update the image in config/manager/kustomization.yaml (or use
# `kustomize edit set image`) and re-apply.
```

### 2. Grant workload-cluster RBAC

The controller talks to each workload cluster using the kubeconfig Secret. If that credential is a
**cluster-admin** (as CAPI's default machine-provisioner kubeconfigs are), skip this step.

For **least-privilege** deployments, apply the workload role to each workload cluster and bind it to
whatever identity the kubeconfig uses:

```bash
kubectl --kubeconfig <workload-kubeconfig> apply -f config/rbac/workload_role.yaml
# Edit the ClusterRoleBinding subject in that file to match the kubeconfig identity.
```

The role grants: read/patch/delete on `nodes.longhorn.io`, read on `replicas`/`volumes`
(longhorn.io), plus Pod list/delete, PVC read, and Node cordon for the EvictStuckPods feature.

### 3. Verify

```bash
kubectl -n longhorn-capi-system get pods
kubectl -n longhorn-capi-system logs deploy/longhorn-capi-eviction-controller -f
```

---

## Configuration

All flags have environment-variable equivalents (same name, upper-snake). They are set on the
**controller Deployment** in the management cluster — either in `config/manager/manager.yaml`
(`args:` or `env:`) or with `kubectl -n longhorn-capi-system edit deploy longhorn-capi-eviction-controller`.
They apply **globally**: one controller watches every workload cluster, so a flag value is not
per-cluster.

| Flag | Env | Default | Description |
|---|---|---|---|
| `--eviction-timeout` | `EVICTION_TIMEOUT` | `2h` | Backstop: release the hook after this long regardless of state. **Must exceed your largest single-volume rebuild time** — the Longhorn KB recommends ≥4h for ~500 GB volumes on on-prem hardware. With early release active, this is rarely hit. |
| `--poll-interval` | `POLL_INTERVAL` | `15s` | Reconcile cadence while a hook is held. |
| `--longhorn-namespace` | `LONGHORN_NAMESPACE` | `longhorn-system` | Where Longhorn lives in workload clusters. |
| `--gc-interval` | `GC_INTERVAL` | `5m` | Sweep cadence for orphaned `nodes.longhorn.io` CRs. |
| `--early-release` | `EARLY_RELEASE` | `true` | Release the hook before rebuild completes when every affected volume is degraded-but-safe (≥ want−1 live replicas, none faulted). See [Early release](#early-release-safety) below. |
| `--evict-stuck-pods` | `EVICT_STUCK_PODS` | `true` | After hook release: cordon the departing node and delete non-DaemonSet pods holding non-faulted Longhorn PVCs, to clear CAPI's volume-detach stage. |
| `--metrics-bind-address` | `METRICS_BIND_ADDRESS` | `:8080` | Metrics endpoint (`0` disables). |
| `--health-probe-bind-address` | `HEALTH_PROBE_BIND_ADDRESS` | `:8081` | `/healthz`, `/readyz`. |
| `--leader-elect` | `LEADER_ELECT` | `true` | Leader election (Lease, in the controller namespace). |

### Early-release safety

The default `--early-release=true` trades one level of redundancy for speed: instead of waiting for
the full rebuild (which in 1-disk-per-node topologies *cannot start* until the departing node is
gone), the hook is released once every affected volume still holds **≥ want−1 running replicas on
surviving nodes and none is faulted**. Data remains at full replica-2 protection; the rebuild
completes when the replacement node arrives.

Set `--early-release=false` for strict behavior: the hook holds until every volume is at full
replica count, or the timeout fires. Use this if your cluster regularly runs volumes with 1–2
replicas where a further loss is unacceptable.

### Sizing the eviction timeout

With early release on, the timeout only matters when a volume is **faulted** or below want−1
replicas — i.e., genuine trouble. Set it to comfortably exceed the rebuild time of your largest
volume (bandwidth-bound: `volume size / replication bandwidth`). The Longhorn KB's guidance of ≥4h
for ~500 GB volumes on on-prem hardware is a reasonable starting point.

---

## Operating

### Watching a node replacement

```bash
# Hook lifecycle (management cluster):
kubectl get machine <name> -n <ns> -o jsonpath='{.metadata.annotations}' | jq .
#   "pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction": "<RFC3339>"
#   "longhorn-capi.io/hook-released": "<RFC3339>"   (after release; prevents re-arming)

# Longhorn eviction progress (workload cluster):
kubectl -n longhorn-system get nodes.longhorn.io <nodeName> -o yaml
#   spec.allowScheduling → false, spec.evictionRequested → true, diskStatus drains

# Events emitted on the Machine:
#   Normal  LonghornEvictionComplete   eviction drained + rebuild gate satisfied
#   Normal  LonghornEarlyRelease       released early (volumes degraded-but-safe)
#   Normal  LonghornStuckPodEvicted    force-deleted pods to free a stuck attachment
#   Normal  LonghornNodeGC             removed orphaned nodes.longhorn.io CRs
#   Warning LonghornEvictionTimeout    backstop fired — investigate why
```

### Troubleshooting

- **Machines stuck in `WaitingForPreTerminateHook`** — check the controller log. If it logs
  `Failed to get Longhorn Node CR` or workload-client errors, the workload API is unreachable; the
  timeout will eventually release, but fixing connectivity is better.
- **Node stuck joining after replacement** — not this controller (its job ends at hook release).
  Check `journalctl -u rancher-system-agent` on the VM: a
  *"secret received was older than the last secret operated on"* plan-sync wedge is a known
  rancher-system-agent behavior that self-recovers after its own restart (~20–30 min).
- **`WaitingForVolumeDetach` for >2 min after hook release** — a workload pod holds the attachment.
  With `--evict-stuck-pods` on, the controller clears it automatically; if it's disabled, drain the
  pods manually or delete the pod.
- **Volume stuck degraded with zero rebuild** — known Longhorn v1.12 issues
  ([#13629](https://github.com/longhorn/longhorn/issues/13629)); check Longhorn's own conditions and
  `LonghornReplicaRebuild` events, not this controller.

---

## Validation

The controller has been validated end-to-end in a Harvester + Rancher + RKE2 lab (single Harvester
host, RKE2 workload clusters, Longhorn 1.11.x) across these scenarios, all with **zero data loss and
zero manual intervention**:

| Scenario | Result |
|---|---|
| Rolling OS-image upgrade, 3 workers, dedicated Longhorn data disks | ✅ 2 hops, SL Micro 6.0→6.1→6.2 |
| Rolling OS-image upgrade, 3 workers, **single-disk** (replicas on the boot disk) | ✅ 2 hops — full rebuild-from-replicas each hop |
| Rolling upgrade, **5 workers** (odd) and **6 workers** (even) | ✅ sequential roll, identical behavior |
| **Concurrent** deletion of 2 workers | ✅ independent per-machine reconcile, parallel join |
| Worker deleted **during an active replica rebuild** of a degraded volume | ✅ safe — no faulted-volume edge hit |
| Multiple volumes per cluster (5Gi + 10Gi + 20Gi) through replacement | ✅ gated independently, all healthy |
| k8s **version-only** upgrade (RKE2 v1.34→v1.36) | ✅ in-place; controller correctly idle |
| Controller pod restart before/during/after eviction | ✅ state recovered from Machine annotations |
| Longhorn node-CR garbage collection (stuck finalizers) | ✅ orphans swept within `--gc-interval` |
| Node-CR GC on clusters **without** Longhorn | ✅ skipped gracefully |

Timing observations from that environment: worker replacement averages ~7–12 min (eviction →
release → machine delete → replacement Ready → volume healthy), dominated by VM provisioning and
join, not Longhorn rebuild. Known lab limits: single-host storage bandwidth, ≤23 GB of replicated
data, and no multi-management-cluster scale testing.

---

## Development

```bash
go vet ./...
go build ./...
go test ./internal/controller/ -v   # 11 unit tests covering the safety invariants
docker build -t longhorn-capi-controller:dev .
```

The unit tests (`internal/controller/*_test.go`) cover the safety-critical invariants:

- Node GC never touches a Longhorn node whose k8s Node exists (even NotReady)
- GC unsticks stuck-deleting CRs and deletes orphans
- Early release fires only on degraded-but-safe; **holds** on faulted volumes, replica deficits, and
  already-complete volumes
- EvictStuckPods deletes healthy-volume pods, **never** DaemonSet pods or pods holding faulted
  volumes

`prompt.md` in the repo root is the original design specification the Longhorn KB motivated — kept
for provenance (it is git-ignored; see `notes.md` for the development log).

**Note on Longhorn types:** `github.com/longhorn/longhorn-manager` v1.8.1 pulls an unresolvable
`k8s.io/kubernetes` dependency, so this repo vendors `k8s/pkg/apis/longhorn/**` under
`third_party/longhorn-manager` with a `replace` directive.

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
