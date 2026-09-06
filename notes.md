All deliverables complete — **verified with real tool output** (`go mod tidy` → `0`, `go vet ./...` → `0`, `go build ./...` → `0`).

### Project layout — `/Users/cwillis/projects/longhorn-capi-controller`

```
.
├── cmd/main.go                                          # Manager, flags/env, scheme, leader election, health probes
├── internal/controller/longhornmachineeviction_controller.go  # Full state machine
├── config/rbac/role.yaml                                # Management ClusterRole/Binding + ServiceAccount + leader-election Role
├── config/rbac/workload_role.yaml                       # Workload ClusterRole/Binding for nodes.longhorn.io
├── config/manager/manager.yaml                          # Namespace + Deployment (4h timeout, probes, securityContext)
├── config/manager/kustomization.yaml                    # Kustomize overlay
├── Dockerfile                                           # Multi-stage golang:1.27 → distroless/static:nonroot
├── README.md                                            # Install, config table, verification recipes, known issues
├── third_party/longhorn-manager/                        # Vendored CRD types (replace directive — avoids k8s.io/kubernetes v0.0.0)
└── prompt.md
```

### What each piece does

- **Controller** (`HookAnnotation = pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction`): watches `Machine` with `deletionTimestamp`, registers hook as RFC3339 timestamp, patches Longhorn `Node` in workload cluster (`allowScheduling=false, evictionRequested=true`), polls `diskStatus.{scheduledReplica,scheduledBackingImage}==0`, releases hook on completion **or** timeout (emits `LonghornEvictionComplete` / `LonghornEvictionTimeout` Events, never blocks CAPI permanently). Handles `NodeRef==nil`, 404, unreachable workload, restarts via timestamp, concurrent Machines, per-disk `V(1)` logging.
- **Manager** — `EvictionTimeout` 2h default (Deployment sets 4h per KB 500GB guidance), `PollInterval` 15s, `LonghornNS` longhorn-system, all via `--flag` or `EVICTION_TIMEOUT` etc. env.
- **RBAC** — management: `machines` get/list/watch/patch/update, `secrets` get/list/watch, `events` create/patch + leader-election `leases`; workload: `nodes` + `nodes/status` on `longhorn.io`.
- **Verified build** — `k8s.io/* v0.32.3` + `controller-runtime v0.20.4` + `cluster-api v1.10.4`; Longhorn types vendored to avoid unresolvable transitive `k8s.io/kubernetes` import.

**Next:** `kubectl apply --server-side -k config/manager` (management), `kubectl --kubeconfig workload apply -f config/rbac/workload_role.yaml` per workload, then `docker build -t ghcr.io/sneezycats/longhorn-capi-controller:dev .`.

### 0.2-alpha — rebuild-completion gate (adds rebuild awareness; addresses E6/E7 findings)
- Added `rebuildComplete()` to the eviction controller: after the departing node drains
  (`isEvictionComplete`), it now ALSO verifies every affected Volume reached
  `spec.numberOfReplicas` running replicas on SURVIVING nodes before releasing the pre-terminate
  hook. Rationale: classic eviction-only releases the hook while the replacement replica set is
  still degraded/stalled — the exact failure recorded in longhorn-maintenance-behavior.md E6/E7.
- Derives affected volumes by listing `Replica`s with `spec.nodeID == departingNode && active`,
  maps replica→volume via `engineName` (strip `-e-<n>`) fallback name `-r-<hash>`.
- Counts live replicas per volume as `running && FailedAt==''` on nodes != departing.
- Requires `volume.spec.numberOfReplicas` satisfied (>=) and volume robustness != `faulted`.
- RBAC: added get/list/watch on longhorn.io `replicas` + `volumes` to `workload_role.yaml`.
- Verified: `go build ./...` and `go vet ./...` clean.
- STILL OPEN (next iterate): the orphaned-`stopped`-replica cleanup on removed nodes (E6/E7: after
  the node's machine is finally deleted, its residual stopped replica remains and can mask/block a
  clean state). The rebuild gate ensures health before CAPI proceeds, but does not yet GC the
  stopped replicas on gone nodes. Consider: watch Machine deletion-complete + evictionRequested
  node, then delete stopped replicas whose node's longhorn Node CR is gone.

### 0.3-alpha — deleting-node replica awareness + force-rebuild (addresses the "healthy but on a doomed node" case)
- Realized the user's finer concern: a volume can report 3 healthy replicas while one still lives on
  a node CAPI is deleting. The 0.2 gate only required ">= number-of-replicas running on surviving
  nodes" which does NOT catch a replica that is (still) on a deleting node.
- New behavior in `rebuildComplete()`:
  1. Build the cluster-wide set of deleting nodes from management-cluster Machines with
     deletionTimestamp + non-empty status.nodeRef (`deletingNodeNames`).
  2. Treat a node as "doomed" if it is the departing node OR in the deleting set.
  3. If any RUNNING replica sits on a doomed node, DELETE it (longhorn.io replicas `delete` RBAC
     added). This forces Longhorn to schedule a fresh replica on a node that doesn't already hold one.
  4. Only release the hook once every affected Volume has spec.numberOfReplicas running replicas on
     NON-doomed nodes and robustness != faulted.
- 0.2 vs 0.3: 0.2 checked the count on surviving nodes; 0.3 additionally detects a replica still on
  a deleting node and proactively deletes it (the manual step from E6). This is the difference that
  makes the upgrade fully autonomour without manual replica deletion.
- RBAC: workload role now `delete` on longhorn.io `replicas`. Management machines get/list/watch already present.
- Verified: `go build ./...`, `go vet ./...` clean.
