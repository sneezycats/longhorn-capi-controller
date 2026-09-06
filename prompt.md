================================================================================
CODING PROMPT: Longhorn CAPI Pre-Termination Hook Controller
================================================================================

## Project Context

We are operating an on-premises cloud platform built on:
  - Rancher (cluster management, Fleet GitOps)
  - Harvester HCI (hypervisor, based on KubeVirt + k3s)
  - Cluster API (CAPI) for guest cluster lifecycle management
  - Longhorn (CSI storage) deployed in guest/workload clusters

When CAPI performs a rolling node replacement (e.g., triggered by Rancher Fleet
updating a MachineDeployment with a new OS image), it deletes old Machine CRs and
provisions new ones. Worker nodes host Longhorn replica data under
/var/lib/longhorn. When a node VM is terminated without prior Longhorn eviction,
all replicas on that node are permanently destroyed. If multiple nodes are replaced
concurrently, this causes catastrophic data loss.

The official Longhorn KB article documenting this problem and prescribing the
solution is:
  https://longhorn.io/kb/how-to-evict-node-during-capi-node-rolling-replacement/

The Longhorn maintainers explicitly declined to build the controller themselves
(GitHub Issue #12871, closed: "Per discussion, we don't implement such operator.
Provided the KB as the development guide instead.") and directed implementers to
use the KB as their build specification.

## Goal

Build a Kubernetes controller, written in Go using controller-runtime, that runs
in the CAPI management cluster (the Rancher/Harvester management plane) and
automates safe Longhorn node eviction before CAPI deletes any Machine CR.

The controller must require zero human intervention. Once deployed, rolling node
replacements initiated by CAPI/Fleet must complete safely without data loss,
regardless of volume size or cluster size.

================================================================================
## CAPI Pre-Termination Hook Mechanism
================================================================================

Reference: https://cluster-api.sigs.k8s.io/tasks/automated-machine-management/machine_deletions

CAPI Machine deletion proceeds through these phases in order:
  1. deletionTimestamp set on Machine CR
  2. Pre-drain hooks complete (annotation prefix: pre-drain.delete.hook.machine.cluster.x-k8s.io)
  3. Node drain
  4. Volume detach wait
  5. Pre-terminate hooks complete  ← THIS IS WHERE OUR CONTROLLER ACTS
  6. InfrastructureMachine deleted
  7. BootstrapConfig deleted
  8. Node object deleted

Pre-terminate hooks are registered by adding an annotation to the Machine CR with
the prefix:
  pre-terminate.delete.hook.machine.cluster.x-k8s.io

The CAPI Machine controller blocks at step 5 and does NOT proceed until ALL
annotations matching that prefix are removed from the Machine CR.

Our controller uses the following annotation key (value can be anything, e.g. ""):
  pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction

Important details:
- The hook fires AFTER Kubernetes pod drain, so Longhorn workload pods may
  already be evicted from the node. Longhorn replica processes (instance managers)
  run as DaemonSets and are excluded from drain, so replicas are still alive.
- There is NO built-in timeout on pre-terminate hooks. The controller must
  implement its own deadline and handle the case where eviction stalls.
- If the hook annotation is never removed, CAPI blocks permanently. The
  controller must always eventually remove it (even on error/timeout) and log
  the reason.
- The Machine CR lives in the MANAGEMENT cluster. The Longhorn Node CR lives in
  the WORKLOAD cluster. The controller must use separate Kubernetes clients for
  each.

================================================================================
## Longhorn Node CR Specification (verified from source)
================================================================================

Source: https://github.com/longhorn/longhorn-manager/blob/master/k8s/pkg/apis/longhorn/v1beta2/node.go

API Group/Version: longhorn.io/v1beta2
Kind: Node
Namespace: longhorn-system

NodeSpec (relevant fields):
  type NodeSpec struct {
      AllowScheduling    bool              `json:"allowScheduling"`
      EvictionRequested  bool              `json:"evictionRequested"`
      Disks              map[string]DiskSpec `json:"disks"`
  }

NodeStatus (relevant fields):
  type NodeStatus struct {
      DiskStatus map[string]*DiskStatus `json:"diskStatus"`
      AutoEvicting bool                 `json:"autoEvicting"`
  }

DiskStatus (relevant fields):
  type DiskStatus struct {
      ScheduledReplica      map[string]int64 `json:"scheduledReplica"`
      ScheduledBackingImage map[string]int64 `json:"scheduledBackingImage"`
  }

Eviction is complete when, for ALL disks in status.diskStatus:
  len(disk.ScheduledReplica) == 0 AND len(disk.ScheduledBackingImage) == 0

Trigger eviction by patching the Longhorn Node CR spec:
  {"spec": {"allowScheduling": false, "evictionRequested": true}}

The Longhorn Node CR name matches the Kubernetes node name in the workload cluster.

================================================================================
## CAPI Machine CR Specification (relevant fields)
================================================================================

API Group/Version: cluster.x-k8s.io/v1beta2  (import as v1beta1 may also exist;
use whichever is current in your CAPI dependency version)
Kind: Machine

Relevant fields:
  metadata.deletionTimestamp     — non-nil means Machine is being deleted
  metadata.annotations           — hook annotation lives here
  spec.clusterName               — name of the CAPI Cluster CR (same namespace)
  status.nodeRef.name            — name of the Kubernetes Node in the workload cluster
                                   (this is also the Longhorn Node CR name)

A Machine is entering deletion when:
  machine.DeletionTimestamp != nil

The hook is registered when:
  machine.Annotations["pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction"]
  exists (any value)

The hook is released by removing that annotation key via PATCH.

================================================================================
## Workload Cluster Client Construction
================================================================================

CAPI stores the workload cluster kubeconfig as a Kubernetes Secret in the
management cluster:
  Name:      <cluster-name>-kubeconfig
  Namespace: <same namespace as the Cluster CR and Machine CR>
  Data key:  "value"  (contains the raw kubeconfig bytes)

To build a controller-runtime or client-go client for the workload cluster:
1. Fetch the Secret from the management cluster using the management cluster client
2. Extract secret.Data["value"]
3. Use clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes) to build a rest.Config
4. Use client.New(restConfig, client.Options{Scheme: workloadScheme}) to build a
   typed client for the workload cluster

The workload cluster client scheme must have Longhorn types registered:
  longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"

================================================================================
## Controller Reconcile Loop (State Machine)
================================================================================

The controller watches Machine CRs in the management cluster.

Trigger condition: machine.DeletionTimestamp != nil

### State: IDLE (no hook registered, Machine not deleting)
  - Do nothing. Return.

### State: DELETION_DETECTED (DeletionTimestamp set, hook NOT yet registered)
  Actions:
  1. Check if machine.Status.NodeRef is set. If not (node never joined or already
     deleted), skip eviction entirely — remove hook if present, or do nothing.
  2. Add annotation: pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction = ""
     to the Machine CR via PATCH.
  3. Record the time the hook was registered (annotation value can store the
     RFC3339 timestamp, e.g., "2026-08-28T12:00:00Z", to enable timeout detection
     across controller restarts).
  4. Requeue immediately.

### State: HOOK_REGISTERED (DeletionTimestamp set, hook present)
  Actions:
  1. Parse the hook annotation value as timestamp to check timeout.
  2. If timeout exceeded (configurable, default: 2 hours):
     - Log a warning with cluster name, machine name, node name, elapsed time.
     - Remove the hook annotation (release CAPI) — do NOT leave CAPI blocked.
     - Emit a Kubernetes Event on the Machine CR describing the timeout.
     - Return.
  3. Fetch the workload cluster kubeconfig Secret from the management cluster.
  4. Build a workload cluster client.
  5. Fetch the Longhorn Node CR (name = machine.Status.NodeRef.Name, namespace = longhorn-system).
  6. If the Longhorn Node CR does not exist:
     - The node may have already been cleaned up. Remove hook and return.
  7. If spec.evictionRequested is not true:
     - PATCH the Longhorn Node CR: allowScheduling=false, evictionRequested=true.
  8. Check eviction completion:
     - Iterate over all entries in status.diskStatus.
     - Eviction complete if ALL disks have len(ScheduledReplica)==0 AND
       len(ScheduledBackingImage)==0.
  9. If NOT complete: requeue after 15 seconds.
  10. If complete:
      - Remove annotation pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction
        from the Machine CR via PATCH.
      - Emit a Kubernetes Event on the Machine CR: "Longhorn eviction complete, releasing pre-terminate hook."
      - Return (do not requeue).

### Edge Cases to Handle
  - Machine.Status.NodeRef is nil: node never registered or already removed.
    Skip eviction, release hook.
  - Longhorn Node CR not found in workload cluster: eviction not needed.
    Release hook.
  - Workload cluster unreachable (kubeconfig Secret missing or API server down):
    Log error, requeue with backoff. DO NOT release hook prematurely — wait for
    connectivity to restore, up to timeout.
  - Controller restarts mid-eviction: On restart, the hook annotation is still
    present on the Machine CR, so the controller will reconcile back into
    HOOK_REGISTERED state and resume polling. The timestamp in the annotation
    value enables correct timeout calculation across restarts.
  - Multiple Machines deleting simultaneously: Each Machine has its own
    reconciler goroutine. Controller-runtime handles this correctly by default.
    Each Machine builds its own workload cluster client independently.
  - Race on annotation patch: Use optimistic concurrency (resourceVersion) or
    SSA (Server-Side Apply) to avoid lost updates.

================================================================================
## Go Module and Dependencies
================================================================================

Minimum Go version: 1.22 (follow controller-runtime's requirement)

Required dependencies (add to go.mod):

  sigs.k8s.io/controller-runtime        — controller framework
  sigs.k8s.io/cluster-api/api/v1beta1   — CAPI Machine, Cluster types
    (import path may vary by CAPI version; check go.mod of your CAPI installation)
  github.com/longhorn/longhorn-manager   — Longhorn CRD types
  k8s.io/client-go                       — Kubernetes client, kubeconfig loading
  k8s.io/apimachinery                    — meta/v1 types, runtime
  k8s.io/api                             — core/v1 Secret type

Key import paths in code:

  ctrl     "sigs.k8s.io/controller-runtime"
  "sigs.k8s.io/controller-runtime/pkg/client"
  "sigs.k8s.io/controller-runtime/pkg/controller"
  "sigs.k8s.io/controller-runtime/pkg/log"
  "sigs.k8s.io/controller-runtime/pkg/predicate"
  "sigs.k8s.io/controller-runtime/pkg/reconcile"

  clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

  longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"

  corev1   "k8s.io/api/core/v1"
  metav1   "k8s.io/apimachinery/pkg/apis/meta/v1"

  "k8s.io/client-go/tools/clientcmd"

Scheme registration in main.go:
  scheme := runtime.NewScheme()
  _ = clientgoscheme.AddToScheme(scheme)
  _ = clusterv1.AddToScheme(scheme)
  // NOTE: Do NOT add longhornv1beta2 to the management cluster scheme.
  // It is registered separately in the per-workload-cluster scheme.

For the workload cluster client, build a separate scheme:
  workloadScheme := runtime.NewScheme()
  _ = clientgoscheme.AddToScheme(workloadScheme)
  _ = longhornv1beta2.AddToScheme(workloadScheme)

================================================================================
## RBAC Requirements
================================================================================

The controller's ServiceAccount needs:

In the MANAGEMENT cluster:
  - machines.cluster.x-k8s.io: get, list, watch, patch, update
  - secrets (core/v1): get, list, watch  (for kubeconfig secrets)
  - events (core/v1): create, patch      (for emitting events)

In the WORKLOAD cluster (via the kubeconfig stored in the Secret):
  - nodes.longhorn.io: get, list, watch, patch, update
  (in namespace: longhorn-system)

RBAC markers for controller-gen (management cluster):
  //+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;patch;update
  //+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
  //+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

RBAC for workload cluster is configured separately (e.g., ClusterRole/ClusterRoleBinding
applied to each workload cluster, bound to the ServiceAccount referenced by the kubeconfig).

================================================================================
## Controller Setup (main.go outline)
================================================================================

  mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
      Scheme:                  scheme,
      MetricsBindAddress:      ":8080",
      LeaderElectionEnabled:   true,
      LeaderElectionID:        "longhorn-capi-eviction-controller",
  })

  err = (&LonghornEvictionReconciler{
      Client:          mgr.GetClient(),
      Scheme:          mgr.GetScheme(),
      EvictionTimeout: 2 * time.Hour,  // configurable via flag or env
  }).SetupWithManager(mgr)

  SetupWithManager:
    return ctrl.NewControllerManagedBy(mgr).
        For(&clusterv1.Machine{}).
        WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
            m := obj.(*clusterv1.Machine)
            return m.DeletionTimestamp != nil
        })).
        WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
        Complete(r)

================================================================================
## Configuration

Expose these as CLI flags or environment variables:

  --eviction-timeout        duration   Default: 2h
      Maximum time to wait for Longhorn eviction before releasing the hook.
      Must be set larger than the expected rebuild time for the largest volume.
      For 500GB volumes on on-prem hardware, recommend starting at 4h minimum.

  --poll-interval           duration   Default: 15s
      How frequently to re-check Longhorn node status during eviction.

  --longhorn-namespace      string     Default: "longhorn-system"
      Namespace where Longhorn is deployed in workload clusters.

================================================================================
## Deliverables
================================================================================

1. cmd/main.go
   - Manager setup, scheme registration, flag parsing, leader election

2. internal/controller/longhornmachineeviction_controller.go
   - LonghornEvictionReconciler struct
   - Reconcile() implementing the state machine above
   - SetupWithManager()
   - buildWorkloadClusterClient() helper
   - isEvictionComplete() helper
   - emitEvent() helper

3. config/rbac/role.yaml
   - ClusterRole for management cluster permissions

4. config/rbac/workload_role.yaml
   - ClusterRole to be applied to each workload cluster

5. config/manager/manager.yaml
   - Deployment manifest for the controller in the management cluster

6. config/manager/kustomization.yaml
   - Kustomize overlay for deployment

7. Dockerfile
   - Multi-stage Go build

8. README.md
   - Installation instructions
   - Configuration reference
   - How to verify it is working (check Machine annotations during deletion,
     check Longhorn node eviction status, check Events on Machine CR)

================================================================================
## Known Issues / Constraints to Accommodate
================================================================================

1. Longhorn v1.12.x Bug (GitHub Issue #13629, August 2026):
   Some volumes sit degraded with zero rebuild activity even after correct
   eviction. Workaround: isEvictionComplete() should check BOTH diskStatus maps
   AND optionally also verify that no volumes remain in "Degraded" state on the
   node before releasing the hook. This is optional/configurable.

2. Longhorn v1.12.x Bug (GitHub Issue #13571, July 2026):
   V1 volumes may fail to rebuild after cluster shutdown. This affects the post-
   eviction rebuild phase, not the eviction controller itself, but document it
   in README so operators know to watch for stalled rebuilds after node deletion.

3. Anti-affinity stalls:
   If replica anti-affinity rules (node, zone, disk) cannot be satisfied on
   remaining nodes, eviction will stall indefinitely. The timeout mechanism
   handles this. The controller should log disk-by-disk replica counts on each
   poll cycle at debug level so operators can diagnose stalls.

4. Node never registered (status.nodeRef == nil):
   Machine was never fully provisioned. Skip eviction entirely, release hook.

5. Controller must NOT assume Longhorn is installed in every workload cluster.
   If the Longhorn Node CR is not found (404), release the hook gracefully and
   log that Longhorn was not found on this cluster.

================================================================================
## Reference Links (all verified)
================================================================================

Longhorn KB - CAPI eviction procedure:
  https://longhorn.io/kb/how-to-evict-node-during-capi-node-rolling-replacement/

CAPI Machine deletion lifecycle and pre-terminate hooks:
  https://cluster-api.sigs.k8s.io/tasks/automated-machine-management/machine_deletions

CAPI Machine controller internals:
  https://cluster-api.sigs.k8s.io/developer/core/controllers/machine

Longhorn Node v1beta2 Go type source (NodeSpec, NodeStatus, DiskStatus):
  https://github.com/longhorn/longhorn-manager/blob/master/k8s/pkg/apis/longhorn/v1beta2/node.go

controller-runtime repository and documentation:
  https://github.com/kubernetes-sigs/controller-runtime

Kubebuilder book (project scaffolding, reconciler patterns, RBAC markers):
  https://book.kubebuilder.io/quick-start

Longhorn scheduling documentation (anti-affinity, replica placement):
  https://longhorn.io/docs/1.12.0/nodes-and-volumes/nodes/scheduling

Longhorn multiple disk support (disk spec structure):
  https://longhorn.io/docs/latest/nodes-and-volumes/nodes/multidisk/

GitHub Issue #12871 (maintainer decision: no official controller, KB is the guide):
  https://github.com/longhorn/longhorn/issues/12871

GitHub Issue #13629 (active bug: volumes stuck degraded, rebuild stall):
  https://github.com/longhorn/longhorn/issues/13629

GitHub Issue #13571 (active bug: V1 volumes not rebuilding after reboot):
  https://github.com/longhorn/longhorn/issues/13571

================================================================================
## End of Prompt
================================================================================

