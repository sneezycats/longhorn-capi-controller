# longhorn-capi-controller

Kubernetes controller that automates **safe Longhorn node eviction** before Cluster API (CAPI) deletes a `Machine` CR during rolling node replacement. Runs in the **CAPI management cluster** (Rancher / Harvester).

Prevents catastrophic data loss when CAPI replaces worker nodes that host Longhorn replicas under `/var/lib/longhorn`.

> Based on the [Longhorn KB: How to evict node during CAPI node rolling replacement](https://longhorn.io/kb/how-to-evict-node-during-capi-node-rolling-replacement/) and [CAPI Machine deletions](https://cluster-api.sigs.k8s.io/tasks/automated-machine-management/machine_deletions).  
> Longhorn maintainers closed [#12871](https://github.com/longhorn/longhorn/issues/12871) — *"we don't implement such operator, KB is the guide."* This repo is that implementation.

---

## How it works

CAPI blocks `Machine` deletion at the **pre-terminate hook** until every annotation with prefix `pre-terminate.delete.hook.machine.cluster.x-k8s.io/` is removed. This controller:

1. Watches `Machine` CRs for `deletionTimestamp != nil`.
2. Registers `pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction=<RFC3339>` via `PATCH` (timestamp survives restarts for timeout).
3. Patches the Longhorn `Node` CR in the **workload cluster** (`longhorn-system/<nodeName>`) to `{allowScheduling:false, evictionRequested:true}`.
4. Polls every `--poll-interval` (15 s) until **all** disks report `len(ScheduledReplica)==0 && len(ScheduledBackingImage)==0`.
5. **Rebuild-completion gate + deleting-node cleanup (NEW):** draining the node isn't enough — the replicas that lived there must be **fully rebuilt onto non-deleting nodes** before CAPI proceeds. The controller derives the set of CAPI-deleting nodes (Machines with `deletionTimestamp` + `nodeRef`). If any running replica still sits on a deleting node, the controller **deletes that replica** to force Longhorn to schedule a fresh one on a node that doesn't already hold one; it then waits until every affected `Volume` reaches `spec.numberOfReplicas` running replicas on non-deleting nodes (robustness not `faulted`). This closes the observed gap where a volume reports 3 "healthy" replicas while one still lives on a node scheduled for deletion — the count looks satisfied but the rebuild never leaves the doomed node. Releasing the hook would otherwise let CAPI finish while the replacement node comes up with a degraded/zero-replica set.
6. Removes the hook annotation → CAPI proceeds to `InfrastructureMachine` → `BootstrapConfig` → `Node` deletion.
7. If eviction OR rebuild stalls past `--eviction-timeout` (default 2 h, **recommend 4 h+** for ≥500 GB volumes), emits a `Warning` Event and releases the hook anyway so CAPI is never blocked forever.

Per-disk replica counts are logged at `V(1)` for stall diagnosis. Each `Machine` reconciles independently (max 10 concurrent).

---

## Installation

### Prerequisites

- Management cluster with CAPI `Machine` CRDs installed.
- `kubectl` with a kubeconfig for the management cluster.
- Workload cluster kubeconfigs stored as Secrets `<cluster>-kubeconfig` in the same namespace as the `Machine`/`Cluster` CRs (standard CAPI convention, `data.value` holds raw kubeconfig).

### 1. Deploy to the management cluster

```bash
# From repo root — creates namespace, RBAC, and Deployment:
kubectl apply --server-side -k config/manager

# Or build + push your own image first:
docker build -t ghcr.io/<org>/longhorn-capi-controller:$(git rev-parse --short HEAD) .
docker push ghcr.io/<org>/longhorn-capi-controller:$(git rev-parse --short HEAD)
# then override the image in the kustomization:
kustomize edit -n config/manager set image ghcr.io/sneezycats/longhorn-capi-controller=ghcr.io/<org>/longhorn-capi-controller:$(git rev-parse --short HEAD)
kubectl apply --server-side -k config/manager
```

### 2. Grant workload-cluster RBAC

The kubeconfig stored in `<cluster>-kubeconfig` authenticates as a ServiceAccount in the **workload** cluster. That identity needs permission to read/patch `nodes.longhorn.io` in `longhorn-system`.

```bash
# Apply to EACH workload cluster that runs Longhorn:
kubectl --kubeconfig <workload-kubeconfig> apply -f config/rbac/workload_role.yaml
# Edit the ClusterRoleBinding subject if the kubeconfig identity differs from
# longhorn-capi-system/longhorn-capi-eviction-controller.
```

### 3. Verify the controller is running

```bash
kubectl -n longhorn-capi-system get pods -l control-plane=longhorn-capi-eviction-controller
kubectl -n longhorn-capi-system logs deploy/longhorn-capi-eviction-controller -f
```

---

## Configuration

| Flag / Env | Default | Description |
|---|---|---|
| `--eviction-timeout` / `EVICTION_TIMEOUT` | `2h` | Max time to wait for eviction before releasing the hook. **Set ≥4h for 500 GB volumes on on-prem hardware.** Must exceed the largest single-volume rebuild time. |
| `--poll-interval` / `POLL_INTERVAL` | `15s` | Interval between Longhorn Node status checks. |
| `--longhorn-namespace` / `LONGHORN_NAMESPACE` | `longhorn-system` | Namespace where Longhorn is deployed in workload clusters. |
| `--metrics-bind-address` / `METRICS_BIND_ADDRESS` | `:8080` | Metrics endpoint. `0` to disable. |
| `--health-probe-bind-address` / `HEALTH_PROBE_BIND_ADDRESS` | `:8081` | `/healthz` and `/readyz`. |
| `--leader-elect` / `LEADER_ELECT` | `true` | Leader election (`leases`, id `longhorn-capi-eviction-controller`). |

Edit `config/manager/manager.yaml` `args:` or set env vars on the Deployment.

---

## Verification

Trigger a rolling replacement (e.g. Fleet updates a `MachineDeployment` image) and observe:

### Machine annotation

```bash
# While eviction is in progress this annotation is present with an RFC3339 timestamp:
kubectl get machine <machine-name> -n <ns> -o jsonpath='{.metadata.annotations}' | jq .
# expect: {"pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction":"2026-08-28T12:00:00Z"}

# After completion the annotation is removed and deletion proceeds:
kubectl get machine <machine-name> -n <ns>   # eventually NotFound
```

### Longhorn Node eviction status (workload cluster)

```bash
kubectl --kubeconfig <workload-kubeconfig> -n longhorn-system get node <nodeName> -o yaml
# spec.allowScheduling should flip to false
# spec.evictionRequested should flip to true
# status.diskStatus[*].scheduledReplica / scheduledBackingImage drain to {}
kubectl --kubeconfig <workload-kubeconfig> -n longhorn-system get lhn <nodeName> -o jsonpath='{.status.diskStatus}' | jq .
```

### Events on the Machine CR

```bash
kubectl get events -n <ns> --field-selector involvedObject.name=<machine-name>
# Normal  LonghornEvictionComplete  Longhorn eviction complete for node <n>, releasing pre-terminate hook
# Warning LonghornEvictionTimeout   Longhorn eviction timed out after 2h0m0s for node <n>; releasing hook
```

### Controller logs

```bash
kubectl -n longhorn-capi-system logs deploy/longhorn-capi-eviction-controller -f
# Increase verbosity: add --zap-log-level=1 (or --zap-devel) to args for V(1) per-disk counts.
```

---

## Edge cases

- **No `status.nodeRef`** — Machine never joined or already removed. No hook is set / hook is released immediately.
- **Longhorn not installed** (`Node` CR 404) — hook released immediately.
- **Workload cluster unreachable** — requeues at `poll-interval`, never releases early; timeout is the backstop.
- **Controller restart** — timestamp in the annotation restores correct timeout accounting.
- **Concurrent Machines** — each Machine has its own reconcile loop, independent workload clients.
- **Anti-affinity stalls** — if replica anti-affinity cannot be satisfied on remaining nodes, eviction stalls until timeout. Check `V(1)` logs and Longhorn volume conditions; consider relaxing hard anti-affinity or adding capacity.

---

## Known issues

- **Longhorn v1.12 — volumes stuck Degraded with zero rebuild** ([#13629](https://github.com/longhorn/longhorn/issues/13629), Aug 2026). Symptom: eviction completes but replicas never reschedule. Workaround is not in this controller; monitor Longhorn volume conditions after eviction.
- **Longhorn v1.12 — V1 volumes may not rebuild after cluster shutdown** ([#13571](https://github.com/longhorn/longhorn/issues/13571), Jul 2026). Affects post-eviction rebuild, not eviction itself. Watch for stalled rebuilds after the node is finally deleted.
- **Multiple disks** — eviction is only complete when **every** `status.diskStatus` entry is empty, per [Longhorn multi-disk docs](https://longhorn.io/docs/latest/nodes-and-volumes/nodes/multidisk/).

---

## Development

```bash
go vet ./...
go build ./...             # requires Go 1.22+
docker build -t ghcr.io/sneezycats/longhorn-capi-controller:dev .
```

Third-party note: `github.com/longhorn/longhorn-manager` v1.8.1 imports `k8s.io/kubernetes v0.0.0` which is unresolvable. This repo vendors only `k8s/pkg/apis/longhorn/**` under `third_party/longhorn-manager` with a `replace` directive so `go mod tidy` stays clean without pulling the full Longhorn manager.

## License

Apache 2.0 — same as CAPI and Longhorn.
