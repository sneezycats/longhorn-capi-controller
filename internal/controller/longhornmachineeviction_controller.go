package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
)

// HookAnnotation is the CAPI pre-terminate hook annotation key.
const HookAnnotation = "pre-terminate.delete.hook.machine.cluster.x-k8s.io/longhorn-node-eviction"

// HookReleasedAnnotation records that we released the hook once for this
// deletion. CAPI's deletion of the Machine may take a while after release
// (e.g. when infra deletion stalls); without this marker the reconciler would
// re-register the hook on every subsequent reconcile — re-arming the very
// gate it just released (observed in E10: "Eviction timeout exceeded" followed
// by endless re-registration while the machine sat in Deleting).
const HookReleasedAnnotation = "longhorn-capi.io/hook-released"

// LonghornEvictionReconciler reconciles CAPI Machine objects to ensure
// Longhorn replicas are fully evicted before the Machine is terminated.
//
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type LonghornEvictionReconciler struct {
	client.Client
	// APIReader is the manager's uncached reader, used exclusively to read
	// workload kubeconfig Secrets. Routing Secret reads through the cached
	// client would lazily start a Secret informer that lists and caches every
	// Secret in the management cluster — widening the required RBAC to
	// list/watch and keeping all workload admin credentials in memory.
	APIReader       client.Reader
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	EvictionTimeout time.Duration
	PollInterval    time.Duration
	LonghornNS      string
	// EarlyRelease: when eviction has drained but the rebuild is blocked only
	// by the departing node's membership, release the hook early provided all
	// affected volumes are degraded-but-safe (>= want-1 running replicas, none
	// faulted). Lets CAPI finish the machine delete so the GC can remove the
	// stale Longhorn Node CR and the rebuild can proceed on the replacement.
	EarlyRelease bool
	// EvictStuckPods: after the hook is released, if the Machine lingers at
	// CAPI's WaitingForVolumeDetach because a workload pod still holds a
	// Longhorn volume attached to the departing node, cordon the node and
	// force-delete those pods so the volume detaches. Only pods whose volumes
	// are healthy on surviving replicas are touched.
	EvictStuckPods bool
}

// SetupWithManager registers the controller with the manager.
func (r *LonghornEvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			m, ok := obj.(*clusterv1.Machine)
			if !ok {
				return false
			}
			return m.DeletionTimestamp != nil
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}

// Reconcile implements the state machine described in prompt.md.
func (r *LonghornEvictionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	machine := &clusterv1.Machine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// IDLE: not deleting → nothing to do.
	if machine.DeletionTimestamp == nil || machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	_, hookExists := machine.Annotations[HookAnnotation]

	// State: DELETION_DETECTED — hook not yet registered.
	if !hookExists {
		return r.handleDeletionDetected(ctx, machine)
	}

	// State: HOOK_REGISTERED — hook present, drive eviction.
	return r.handleHookRegistered(ctx, machine)
}

// handleDeletionDetected covers the case where DeletionTimestamp is set but the hook
// annotation has not yet been added.
func (r *LonghornEvictionReconciler) handleDeletionDetected(ctx context.Context, machine *clusterv1.Machine) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// State: DELETION_DETECTED — hook not yet registered. But if we already
	// released the hook once for this deletion, never re-register: CAPI is
	// free to finish deleting the machine, and re-arming the hook here would
	// wedge the deletion (observed in E10).
	if machine.Annotations[HookReleasedAnnotation] != "" {
		l.Info("Hook already released for this deletion — not re-registering", "machine", machine.Name)
		// 0.8: the hook is gone but the Machine may linger at CAPI's native
		// WaitingForVolumeDetach stage (observed on every rolling hop in E12):
		// a workload pod still Running on the departing node holds the RWO
		// volume's ATTACHMENT (attachments follow pods, not replicas). If
		// enabled, cordon the node and force-delete Longhorn-PVC-backed pods
		// so the volume detaches and CAPI can finish.
		if r.EvictStuckPods {
			// Build the workload client here; the release path may run long
			// after the hook reconciliation that had one.
			if wlClient, err := buildWorkloadClusterClient(ctx, r.APIReader, machine.Spec.ClusterName, machine.Namespace); err == nil {
				r.releaseStuckAttachments(ctx, wlClient, machine)
			} else {
				log.FromContext(ctx).V(1).Info("EvictStuckPods: no workload client yet", "machine", machine.Name, "err", err.Error())
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// If NodeRef is nil the node never joined or was already removed — nothing to evict.
	if machine.Status.NodeRef == nil || machine.Status.NodeRef.Name == "" {
		l.Info("Machine has no NodeRef — skipping Longhorn eviction", "machine", machine.Name)
		return ctrl.Result{}, nil
	}

	// Register the pre-terminate hook with a timestamp value for timeout tracking.
	now := time.Now().UTC().Format(time.RFC3339)
	patch := client.MergeFrom(machine.DeepCopy())
	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}
	machine.Annotations[HookAnnotation] = now
	if err := r.Patch(ctx, machine, patch); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		l.Error(err, "Failed to add pre-terminate hook annotation", "machine", machine.Name)
		return ctrl.Result{}, err
	}
	l.Info("Registered Longhorn pre-terminate hook", "machine", machine.Name, "node", machine.Status.NodeRef.Name, "timestamp", now)
	return ctrl.Result{Requeue: true}, nil
}

// handleHookRegistered drives eviction once the hook is present.
func (r *LonghornEvictionReconciler) handleHookRegistered(ctx context.Context, machine *clusterv1.Machine) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	nodeName := ""
	if machine.Status.NodeRef != nil {
		nodeName = machine.Status.NodeRef.Name
	}
	clusterName := machine.Spec.ClusterName

	// 1. Timeout check.
	hookValue := machine.Annotations[HookAnnotation]
	hookTime, err := time.Parse(time.RFC3339, hookValue)
	if err != nil {
		// Backwards compat: empty string or unparsable → treat as now.
		l.Info("Hook annotation value is not a valid RFC3339 timestamp, using current time as fallback", "machine", machine.Name, "value", hookValue)
		hookTime = time.Now().UTC()
	}
	elapsed := time.Since(hookTime)
	if elapsed > r.EvictionTimeout {
		l.Info("Eviction timeout exceeded — releasing hook to unblock CAPI",
			"machine", machine.Name, "cluster", clusterName, "node", nodeName,
			"elapsed", elapsed, "timeout", r.EvictionTimeout)
		r.emitEvent(machine, corev1.EventTypeWarning, "LonghornEvictionTimeout",
			fmt.Sprintf("Longhorn eviction timed out after %s for node %s; releasing pre-terminate hook to unblock CAPI deletion", elapsed.Truncate(time.Second), nodeName))
		if err := r.removeHook(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 2. Handle missing NodeRef post-hook (edge: node deleted after hook was set).
	if nodeName == "" {
		l.Info("Machine NodeRef is nil while hook is registered — releasing hook", "machine", machine.Name)
		if err := r.removeHook(ctx, machine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 3. Build workload cluster client.
	wlClient, err := buildWorkloadClusterClient(ctx, r.APIReader, clusterName, machine.Namespace)
	if err != nil {
		l.Error(err, "Failed to build workload cluster client — requeueing", "machine", machine.Name, "cluster", clusterName)
		// Do NOT release hook — wait for connectivity, up to timeout.
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	// 4. Fetch Longhorn Node CR.
	lhNode := &longhornv1beta2.Node{}
	key := types.NamespacedName{Name: nodeName, Namespace: r.LonghornNS}
	if err := wlClient.Get(ctx, key, lhNode); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("Longhorn Node CR not found — assuming Longhorn not installed or already cleaned up, releasing hook",
				"machine", machine.Name, "node", nodeName, "cluster", clusterName)
			if err := r.removeHook(ctx, machine); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		l.Error(err, "Failed to get Longhorn Node CR — requeueing", "machine", machine.Name, "node", nodeName)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	// 5. Trigger eviction if not already requested.
	if !lhNode.Spec.EvictionRequested {
		l.Info("Triggering Longhorn eviction", "machine", machine.Name, "node", nodeName)
		patch := client.MergeFrom(lhNode.DeepCopy())
		lhNode.Spec.AllowScheduling = false
		lhNode.Spec.EvictionRequested = true
		if err := wlClient.Patch(ctx, lhNode, patch); err != nil {
			l.Error(err, "Failed to patch Longhorn Node to request eviction", "node", nodeName)
			return ctrl.Result{RequeueAfter: r.PollInterval}, nil
		}
		// Re-fetch to get updated status.
		if err := wlClient.Get(ctx, key, lhNode); err != nil {
			l.Error(err, "Failed to re-fetch Longhorn Node after eviction patch", "node", nodeName)
			return ctrl.Result{RequeueAfter: r.PollInterval}, nil
		}
	}

	// 6. Debug: log per-disk replica counts at V(1).
	if l.V(1).Enabled() {
		for diskName, ds := range lhNode.Status.DiskStatus {
			if ds != nil {
				l.V(1).Info("Disk status", "node", nodeName, "disk", diskName,
					"scheduledReplicas", len(ds.ScheduledReplica), "scheduledBackingImages", len(ds.ScheduledBackingImage))
			}
		}
	}

	// 7. Check completion — but ALSO run cleanup regardless of drain state.
	// We want to delete bad replicas (stopped on gone nodes, or on any node CAPI is
	// deleting) EVEN IF the evicting node's diskStatus hasn't drained yet. Holding the
	// hook on eviction alone lets a stale stopped replica block until timeout; running
	// cleanup independently unblocks the rebuild promptly.
	delNodes, err := r.deletingNodeNames(ctx, machine)
	if err != nil {
		l.Error(err, "Failed to enumerate deleting nodes — requeueing", "machine", machine.Name, "node", nodeName)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	// dev note: also include the evicting node + any not-ready node as a cleanup target.
	removed, affected, err := r.cleanupBadReplicas(ctx, wlClient, nodeName, delNodes)
	if err != nil {
		l.Error(err, "Failed to evaluate replica cleanup state — requeueing", "machine", machine.Name, "node", nodeName)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	if len(removed) > 0 {
		l.Info("Deleted bad replicas (stopped-on-gone or on deleting node) to force rebuild",
			"machine", machine.Name, "node", nodeName, "removed", removed)
	}

	// Now check eviction drain state. If the node's self-reported diskStatus
	// still lists scheduled replicas, cross-check against the Replica CRs —
	// the authoritative store. When a departing node's longhorn-manager dies
	// mid-eviction its status map freezes with stale entries (E11: replica
	// already evicted, scheduledReplica still listing it), which would hold
	// the hook until timeout. If no live replica CR remains on the node,
	// eviction IS complete regardless of the frozen status.
	if !isEvictionComplete(lhNode) {
		replicasDrained, err := r.nodeReplicasDrained(ctx, wlClient, nodeName)
		if err != nil {
			l.Error(err, "Failed to cross-check replica drain state — requeueing", "machine", machine.Name, "node", nodeName)
			return ctrl.Result{RequeueAfter: r.PollInterval}, nil
		}
		if !replicasDrained {
			l.Info("Longhorn eviction in progress — requeueing", "machine", machine.Name, "node", nodeName, "elapsed", elapsed.Truncate(time.Second))
			return ctrl.Result{RequeueAfter: r.PollInterval}, nil
		}
		l.Info("Node diskStatus stale (manager likely dead) but no replica CRs remain on node — treating eviction as complete",
			"machine", machine.Name, "node", nodeName, "elapsed", elapsed.Truncate(time.Second))
	}

	// Rebuild-completion gate: eviction drained, but the replicas must be REBUILT onto
	// surviving nodes before CAPI proceeds. Wait until every volume has
	// spec.numberOfReplicas running replicas on NON-deleting nodes.
	rebuildComplete, affected, err := r.volumesRebuilt(ctx, wlClient, nodeName, delNodes)
	if err != nil {
		l.Error(err, "Failed to evaluate rebuild state — requeueing", "machine", machine.Name, "node", nodeName)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	if !rebuildComplete {
		// 0.7 EARLY RELEASE: if the rebuild is blocked only by the departing
		// node's own membership (disks unavailable), holding the hook just
		// burns the eviction timeout. Release early when every affected volume
		// is still degraded-safe (want-1 running replicas, not faulted).
		if r.EarlyRelease {
			safe, earlyVolumes, err := r.earlyReleaseSafe(ctx, wlClient, nodeName, delNodes)
			if err != nil {
				l.Error(err, "Failed to evaluate early-release safety — requeueing", "machine", machine.Name, "node", nodeName)
				return ctrl.Result{RequeueAfter: r.PollInterval}, nil
			}
			if safe {
				l.Info("Early release: eviction drained, volumes degraded-but-safe, rebuild blocked only by departing node — releasing hook",
					"machine", machine.Name, "node", nodeName, "affectedVolumes", earlyVolumes, "elapsed", elapsed.Truncate(time.Second))
				r.emitEvent(machine, corev1.EventTypeNormal, "LonghornEarlyRelease",
					fmt.Sprintf("Longhorn eviction drained node %s; volumes degraded-but-safe (rebuild blocked by departing node membership) — releasing pre-terminate hook early", nodeName))
				if err := r.removeHook(ctx, machine); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}
		l.Info("Longhorn eviction drained node but replica rebuild incomplete — holding hook",
			"machine", machine.Name, "node", nodeName, "affectedVolumes", affected, "elapsed", elapsed.Truncate(time.Second))
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	if len(affected) > 0 {
		l.Info("Longhorn replicas rebuilt on surviving nodes — releasing hook",
			"machine", machine.Name, "node", nodeName, "affectedVolumes", affected, "elapsed", elapsed.Truncate(time.Second))
	}

	// 8. Complete — release hook.
	l.Info("Longhorn eviction complete — releasing pre-terminate hook", "machine", machine.Name, "node", nodeName)
	r.emitEvent(machine, corev1.EventTypeNormal, "LonghornEvictionComplete",
		fmt.Sprintf("Longhorn eviction complete for node %s, releasing pre-terminate hook", nodeName))
	if err := r.removeHook(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// isEvictionComplete returns true when all disks have zero scheduled replicas and backing images.
// A nil or empty DiskStatus map means the node has never reported (fresh CR,
// or its longhorn-manager died before reporting anything) — that is UNKNOWN,
// not complete: return false so the Replica-CR cross-check in the reconcile
// path decides instead.
func isEvictionComplete(node *longhornv1beta2.Node) bool {
	if node.Status.DiskStatus == nil || len(node.Status.DiskStatus) == 0 {
		return false
	}
	for _, ds := range node.Status.DiskStatus {
		if ds == nil {
			continue
		}
		if len(ds.ScheduledReplica) != 0 || len(ds.ScheduledBackingImage) != 0 {
			return false
		}
	}
	return true
}

// nodeReplicasDrained cross-checks the departing node's frozen diskStatus against
// the Replica CRs (authoritative). Returns true when the node hosts no RUNNING
// replica of any live volume — i.e. the eviction's actual work is done even if
// the dead node's status map still lists stale scheduledReplica entries.
func (r *LonghornEvictionReconciler) nodeReplicasDrained(ctx context.Context, wlClient client.Client, departingNode string) (bool, error) {
	replicaList := &longhornv1beta2.ReplicaList{}
	if err := wlClient.List(ctx, replicaList, client.InNamespace(r.LonghornNS)); err != nil {
		return false, fmt.Errorf("listing replicas in %s: %w", r.LonghornNS, err)
	}
	for i := range replicaList.Items {
		rep := &replicaList.Items[i]
		if rep.Spec.NodeID != departingNode {
			continue
		}
		// A running replica on the departing node = eviction not drained.
		// (Replicas of deleted volumes linger as stopped; only RUNNING blocks.)
		if isReplicaRunning(*rep) {
			return false, nil
		}
	}
	return true, nil
}

// deletingNodeNames returns the set of node names that CAPI is currently
// deleting in the same workload cluster as the given Machine (machines in the
// same namespace with the same spec.clusterName and a deletionTimestamp).
//
// The per-cluster scoping is a safety requirement, not an optimization: node
// names are hostnames and are NOT unique across the workload clusters managed
// by one management cluster. A machine being deleted in a DIFFERENT cluster
// must never mark a same-named node of this cluster as doomed — that would
// make cleanupBadReplicas force-delete replicas of a healthy node. Within the
// cluster the view is still "any doomed node", not just the Machine that
// triggered this reconcile.
func (r *LonghornEvictionReconciler) deletingNodeNames(ctx context.Context, machine *clusterv1.Machine) (map[string]bool, error) {
	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList, client.InNamespace(machine.Namespace)); err != nil {
		return nil, fmt.Errorf("listing machines in %s: %w", machine.Namespace, err)
	}
	out := map[string]bool{}
	for i := range machineList.Items {
		m := &machineList.Items[i]
		if m.Spec.ClusterName != machine.Spec.ClusterName {
			continue
		}
		if m.DeletionTimestamp == nil || m.DeletionTimestamp.IsZero() {
			continue
		}
		if m.Status.NodeRef != nil && m.Status.NodeRef.Name != "" {
			out[m.Status.NodeRef.Name] = true
		}
	}
	return out, nil
}

// cleanupBadReplicas deletes replicas that are "bad" and thus block a clean rebuild:
//
//	(a) any replica (running or stopped) on a node CAPI is deleting (doomed), and
//	(b) any STOPPED, inactive replica whose node is gone from the workload
//	    cluster — no live Longhorn Node CR AND no k8s Node. Observed as the
//	    stale `stopped` replica that kept a volume at 3 "healthy" replicas
//	    while one never left a doomed node and the rebuild stalled
//	    (longhorn-maintenance-behavior.md E6).
//
// A node that merely reports NotReady, or is unschedulable (allowScheduling=false —
// which this controller itself sets when triggering eviction), is NOT gone: replica
// cleanup follows the same source of truth as the Node GC — the k8s Node list —
// so anything still present in the cluster is left to Longhorn's own eviction.
//
// Safety: a replica is never deleted if that would leave its volume with zero
// running replicas that hold the volume's data (Spec.HealthyAt set) on nodes
// that are not being deleted — i.e. it is the volume's last live copy. A
// running-but-never-synced replacement replica (created by the eviction
// replenishment mid-migration) does NOT count as a surviving copy: Longhorn
// clears HealthyAt before a rebuild and sets it only once the replica goes
// read/write. Single-replica volumes are the obvious case (Harvester's
// default storage class is one): Longhorn's own eviction migrates such a
// replica safely given time, while deleting it destroys the data. Volumes whose
// Volume CR no longer exists are garbage and exempt from this protection.
//
// Returns (removedNames, affectedVolumes). Runs regardless of eviction drain state so
// the rebuild is unblocked promptly rather than waiting out the eviction timeout.
func (r *LonghornEvictionReconciler) cleanupBadReplicas(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (removed []string, affected []string, err error) {
	l := log.FromContext(ctx)

	// Live, scheduling-eligible Longhorn node hostnames (targets for a rebuild).
	liveNodes, err := r.liveNodeNames(ctx, wlClient)
	if err != nil {
		return nil, nil, fmt.Errorf("listing live Longhorn nodes: %w", err)
	}
	// k8s Node names — the authoritative "does this node still exist" signal.
	k8sNodes, err := r.k8sNodeNames(ctx, wlClient)
	if err != nil {
		return nil, nil, fmt.Errorf("listing k8s nodes: %w", err)
	}

	replicaList := &longhornv1beta2.ReplicaList{}
	if err := wlClient.List(ctx, replicaList, client.InNamespace(r.LonghornNS)); err != nil {
		return nil, nil, fmt.Errorf("listing replicas in %s: %w", r.LonghornNS, err)
	}

	// Pass 1: classify deletion candidates and count the replicas each volume
	// keeps on nodes that are not being deleted (the surviving data). A replica
	// only counts as surviving data when it actually HOLDS the volume's data:
	// Longhorn clears Spec.HealthyAt before any rebuild and sets it when the
	// replica goes read/write, so a replacement replica created seconds ago by
	// the eviction's own replenishment is running but holds nothing (HealthyAt
	// empty) until its rebuild completes. Counting mere process-liveness made
	// the guard below drop its refusal one poll after Longhorn started a
	// migration, and the source replica — the only copy holding data — was
	// deleted mid-rebuild (CRIT-1a, observed 2026-10-01 on lhcc-roll1 S2).
	type candidate struct {
		rep  *longhornv1beta2.Replica
		doom bool
	}
	var candidates []candidate
	liveByVolume := map[string]int{}
	affectedVolumes := map[string]bool{}
	for i := range replicaList.Items {
		rep := &replicaList.Items[i]
		volName := volumeNameFromReplica(rep)
		nodeID := rep.Spec.NodeID
		doom := nodeID == departingNode || deletingNodes[nodeID]
		gone := nodeID != "" && !liveNodes[nodeID] && !k8sNodes[nodeID] && !rep.Spec.Active
		if doom || gone {
			affectedVolumes[volName] = true
			candidates = append(candidates, candidate{rep: rep, doom: doom})
			continue
		}
		if replicaHoldsData(*rep) {
			liveByVolume[volName]++
		}
	}

	// Pass 2: delete candidates, refusing to destroy a volume's last live copy.
	for _, c := range candidates {
		rep := c.rep
		volName := volumeNameFromReplica(rep)
		if volName != "" {
			vol := &longhornv1beta2.Volume{}
			volErr := wlClient.Get(ctx, types.NamespacedName{Name: volName, Namespace: r.LonghornNS}, vol)
			if volErr != nil && !apierrors.IsNotFound(volErr) {
				return nil, nil, fmt.Errorf("getting volume %s: %w", volName, volErr)
			}
			if volErr == nil && liveByVolume[volName] == 0 {
				l.Info("Refusing to delete a volume's last live replica — no surviving replica holds the volume's data (HealthyAt); leaving it for Longhorn's own eviction/rebuild",
					"replica", rep.Name, "node", rep.Spec.NodeID, "volume", volName)
				continue
			}
		}
		l.Info("Deleting bad replica to force a clean rebuild",
			"replica", rep.Name, "node", rep.Spec.NodeID, "volume", volName, "doomed", c.doom)
		if err := wlClient.Delete(ctx, rep); err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("deleting bad replica %s: %w", rep.Name, err)
		}
		removed = append(removed, rep.Name)
	}

	names := make([]string, 0, len(affectedVolumes))
	for n := range affectedVolumes {
		names = append(names, n)
	}
	sort.Strings(names)
	return removed, names, nil
}

// liveNodeNames returns the set of Longhorn node hostnames that are currently
// scheduling-eligible (allowScheduling true AND Ready condition true). Used to tell a
// "gone" node (the doomed/removed worker) from a real, usable rebuild target.
func (r *LonghornEvictionReconciler) liveNodeNames(ctx context.Context, wlClient client.Client) (map[string]bool, error) {
	nodeList := &longhornv1beta2.NodeList{}
	if err := wlClient.List(ctx, nodeList, client.InNamespace(r.LonghornNS)); err != nil {
		return nil, fmt.Errorf("listing Longhorn nodes in %s: %w", r.LonghornNS, err)
	}
	out := map[string]bool{}
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if !n.Spec.AllowScheduling {
			continue
		}
		ready := conditionStatus(n.Status.Conditions, "Ready")
		if ready == longhornv1beta2.ConditionStatusTrue {
			out[n.Spec.Name] = true
		}
	}
	return out, nil
}

// k8sNodeNames returns the set of Kubernetes Node names in the workload
// cluster. Replica cleanup uses it as the authoritative existence signal —
// the same source of truth as the Node GC: a node whose k8s Node still
// exists (even NotReady, even cordoned) is never treated as gone.
func (r *LonghornEvictionReconciler) k8sNodeNames(ctx context.Context, wlClient client.Client) (map[string]bool, error) {
	k8sNodeList := &corev1.NodeList{}
	if err := wlClient.List(ctx, k8sNodeList); err != nil {
		return nil, fmt.Errorf("listing k8s nodes: %w", err)
	}
	out := map[string]bool{}
	for i := range k8sNodeList.Items {
		out[k8sNodeList.Items[i].Name] = true
	}
	return out, nil
}

// conditionStatus returns the ConditionStatus of the named condition, or Unknown.
func conditionStatus(conds []longhornv1beta2.Condition, typeName string) longhornv1beta2.ConditionStatus {
	for _, c := range conds {
		if c.Type == typeName {
			return c.Status
		}
	}
	return longhornv1beta2.ConditionStatusUnknown
}

// volumesRebuilt reports whether every volume affected by this eviction has
// spec.numberOfReplicas running replicas on NON-deleting nodes (robustness not faulted).
// This is the final health gate before releasing the pre-terminate hook.
func (r *LonghornEvictionReconciler) volumesRebuilt(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (complete bool, affected []string, err error) {
	names, ok, _, err := r.volumeReplicaState(ctx, wlClient, departingNode, deletingNodes)
	if err != nil || !ok {
		return false, names, err
	}
	return true, names, nil
}

// earlyReleaseSafe reports whether the hook can be released BEFORE the replica
// rebuild finishes without risking data: eviction has drained the departing
// node, and every volume it held replicas for still has (want-1) running,
// non-faulted replicas on surviving nodes — degraded, not faulted. In the
// 3-replica/3-worker/1-disk-per-node topology the rebuild cannot start while
// the dead node remains a schedulable member ("precheck new replica failed:
// disks are unavailable"), so holding the hook just burns the eviction timeout
// (E10). Releasing early lets CAPI delete the machine → the GC removes the
// stale Longhorn Node CR → Longhorn schedules the rebuild on the replacement.
// A faulted volume, a replica deficit beyond max(want-1, 1) data-holding
// replicas, or a want=1 volume without its one data-holding replica still
// holds the hook.
func (r *LonghornEvictionReconciler) earlyReleaseSafe(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (bool, []string, error) {
	names, allComplete, degradedSafe, err := r.volumeReplicaState(ctx, wlClient, departingNode, deletingNodes)
	if err != nil || allComplete {
		return false, names, err
	}
	return degradedSafe, names, nil
}

// volumeReplicaState returns the sorted affected-volume names, whether ALL
// affected volumes already meet their full replica count on live nodes
// (allComplete), and whether they are all degraded-but-safe (>= max(want-1, 1)
// data-holding replicas each and none faulted — degradedSafe). Only replicas
// that hold the volume's data count (replicaHoldsData): a running replacement
// replica that has not finished its rebuild does not. Faulted forces
// degradedSafe=false.
func (r *LonghornEvictionReconciler) volumeReplicaState(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (names []string, allComplete bool, degradedSafe bool, err error) {
	replicaList := &longhornv1beta2.ReplicaList{}
	if err := wlClient.List(ctx, replicaList, client.InNamespace(r.LonghornNS)); err != nil {
		return nil, false, false, fmt.Errorf("listing replicas in %s: %w", r.LonghornNS, err)
	}

	affectedVolumes := map[string]bool{}
	liveByVolume := map[string]int{}
	for _, rep := range replicaList.Items {
		nodeID := rep.Spec.NodeID
		doomed := nodeID == departingNode || deletingNodes[nodeID]
		volName := volumeNameFromReplica(&rep)
		if volName == "" {
			continue
		}
		affectedVolumes[volName] = true
		// Same data-holding rule as cleanupBadReplicas: a running replica that
		// has not completed a rebuild yet (HealthyAt empty) is not a copy the
		// volume can afford to rely on.
		if doomed || !replicaHoldsData(rep) {
			continue
		}
		liveByVolume[volName]++
	}

	names = make([]string, 0, len(affectedVolumes))
	for n := range affectedVolumes {
		names = append(names, n)
	}
	sort.Strings(names)

	allComplete = true
	degradedSafe = true
	for _, volName := range names {
		vol := &longhornv1beta2.Volume{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: volName, Namespace: r.LonghornNS}, vol); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, false, false, fmt.Errorf("getting volume %s: %w", volName, err)
		}
		want := vol.Spec.NumberOfReplicas
		if want <= 0 {
			want = 1
		}
		if liveByVolume[volName] < want {
			allComplete = false
		}
		// Degraded-but-safe needs max(want-1, 1) data-holding replicas. For a
		// want=1 volume, want-1 is 0 and "zero surviving copies" must never
		// count as safe — CRIT-1a: the 2026-10-01 S2 release fired on exactly
		// this degeneracy while the volume's only copy was an unsynced
		// replacement replica.
		minSurviving := want - 1
		if minSurviving < 1 {
			minSurviving = 1
		}
		if liveByVolume[volName] < minSurviving {
			degradedSafe = false
		}
		if vol.Status.Robustness == longhornv1beta2.VolumeRobustnessFaulted {
			return names, false, false, nil
		}
	}
	return names, allComplete, degradedSafe, nil
}

// volumeNameFromReplica derives the Longhorn volume name from a Replica CR.
// Longhorn names replicas "<volume>-r-<hash>" and sets spec.engineName to
// "<volume>-e-0". We prefer spec.engineName minus the trailing "-e-<n>" segment,
// falling back to stripping the "-r-<hash>" suffix from the object name.
func volumeNameFromReplica(rep *longhornv1beta2.Replica) string {
	if rep.Spec.EngineName != "" {
		if idx := strings.LastIndex(rep.Spec.EngineName, "-e-"); idx > 0 {
			return rep.Spec.EngineName[:idx]
		}
		return rep.Spec.EngineName
	}
	if idx := strings.LastIndex(rep.Name, "-r-"); idx > 0 {
		return rep.Name[:idx]
	}
	return rep.Name
}

// isReplicaRunning reports whether a replica is usable (currently running and not failed).
func isReplicaRunning(rep longhornv1beta2.Replica) bool {
	st := rep.Status.InstanceStatus
	// currentState values: running, stopped, error, starting, stopping.
	return st.CurrentState == "running" && rep.Spec.FailedAt == ""
}

// replicaHoldsData reports whether a replica is usable AND currently holds the
// volume's data. Longhorn clears Spec.HealthyAt before a rebuild and sets it
// when the replica goes read/write, so a replacement replica created by the
// eviction's replenishment is running but holds nothing until its rebuild
// completes. "A running process" is not "a surviving copy" — counting unsynced
// replicas as live data destroyed a single-replica volume mid-migration
// (CRIT-1a, 2026-10-01).
func replicaHoldsData(rep longhornv1beta2.Replica) bool {
	return isReplicaRunning(rep) && rep.Spec.HealthyAt != ""
}

// removeHook removes the pre-terminate hook annotation from the Machine CR via PATCH.
// It always re-fetches the latest Machine to avoid stale resourceVersion conflicts.
func (r *LonghornEvictionReconciler) removeHook(ctx context.Context, machine *clusterv1.Machine) error {
	l := log.FromContext(ctx)
	latest := &clusterv1.Machine{}
	if err := r.Get(ctx, types.NamespacedName{Name: machine.Name, Namespace: machine.Namespace}, latest); err != nil {
		return err
	}
	if _, ok := latest.Annotations[HookAnnotation]; !ok {
		return nil // already removed
	}
	patch := client.MergeFrom(latest.DeepCopy())
	delete(latest.Annotations, HookAnnotation)
	// Mark the release so the reconciler never re-arms the hook for this
	// deletion (the re-registration loop observed in E10).
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[HookReleasedAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, latest, patch); err != nil {
		if apierrors.IsConflict(err) {
			// Retry once on conflict.
			if err2 := r.Get(ctx, types.NamespacedName{Name: machine.Name, Namespace: machine.Namespace}, latest); err2 != nil {
				return err2
			}
			if _, ok := latest.Annotations[HookAnnotation]; !ok {
				return nil
			}
			patch2 := client.MergeFrom(latest.DeepCopy())
			delete(latest.Annotations, HookAnnotation)
			if err2 := r.Patch(ctx, latest, patch2); err2 != nil {
				l.Error(err2, "Failed to remove hook annotation (retry)", "machine", machine.Name)
				return err2
			}
			return nil
		}
		l.Error(err, "Failed to remove hook annotation", "machine", machine.Name)
		return err
	}
	l.Info("Removed pre-terminate hook annotation", "machine", machine.Name)
	return nil
}

// releaseStuckAttachments unblocks CAPI's WaitingForVolumeDetach stage after
// the hook has been released: it cordons the departing node and force-deletes
// non-DaemonSet pods that (a) are Running on the departing node, (b) consume a
// PVC that positively resolves to a Longhorn Volume CR (not merely "any PVC"),
// and (c) whose Longhorn volume is NOT faulted (replicas healthy on surviving
// nodes). The pod reschedules to a live node and the volume detaches.
// DaemonSet pods are never touched (they belong on every node by design), and
// a pod whose volume state cannot be verified (lookup error) is skipped:
// never force-delete on unverified state.
func (r *LonghornEvictionReconciler) releaseStuckAttachments(ctx context.Context, wlClient client.Client, machine *clusterv1.Machine) {
	l := log.FromContext(ctx)
	nodeName := ""
	if machine.Status.NodeRef != nil {
		nodeName = machine.Status.NodeRef.Name
	}
	if nodeName == "" {
		return
	}

	// 1. Cordon the departing node so evicted pods do not land back on it.
	k8sNode := &corev1.Node{}
	if err := wlClient.Get(ctx, types.NamespacedName{Name: nodeName}, k8sNode); err == nil {
		if !k8sNode.Spec.Unschedulable {
			patch := client.MergeFrom(k8sNode.DeepCopy())
			k8sNode.Spec.Unschedulable = true
			if err := wlClient.Patch(ctx, k8sNode, patch); err != nil {
				l.Error(err, "EvictStuckPods: failed to cordon departing node", "node", nodeName)
			} else {
				l.Info("EvictStuckPods: cordoned departing node", "node", nodeName)
			}
		}
	} else if apierrors.IsNotFound(err) {
		return // node already gone; nothing to release
	}

	// 2. Find pods on the node holding Longhorn PVCs. Both the field-selector
	// path and the full-list fallback can fail transiently when the workload
	// API is churning mid-roll (control plane itself being replaced — observed
	// in E15). Retry with a short bounded backoff before giving up this cycle.
	podList, err := r.listPodsWithRetry(ctx, wlClient, nodeName)
	if err != nil {
		l.Error(err, "EvictStuckPods: failed to list pods after retries", "node", nodeName)
		return
	}

	evicted := []string{}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}
		if isDaemonSetPod(pod) {
			continue
		}
		if !podHasAnyPVC(pod) {
			continue
		}
		// Only Longhorn-backed pods are ours to evict: this feature exists to
		// release Longhorn attachments, and force-deleting unrelated
		// workloads is out of scope. Skip if any Longhorn volume is faulted,
		// and skip on lookup errors — never evict on unverified state.
		hasLonghorn, faulted, err := r.podLonghornVolumeState(ctx, wlClient, pod)
		if err != nil {
			l.Error(err, "EvictStuckPods: cannot verify pod volumes — skipping pod",
				"pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		if !hasLonghorn {
			continue
		}
		if faulted {
			l.Info("EvictStuckPods: pod holds a FAULTED Longhorn volume — not evicting (safety)",
				"pod", pod.Name, "namespace", pod.Namespace, "node", nodeName)
			continue
		}
		// Grace period 0 = actual force-delete. A plain Delete on a node whose
		// kubelet is gone leaves the pod Terminating forever — the exact stuck
		// state this is meant to clear.
		if err := wlClient.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			l.Error(err, "EvictStuckPods: failed to delete pod", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		evicted = append(evicted, pod.Namespace+"/"+pod.Name)
	}

	if len(evicted) > 0 {
		l.Info("EvictStuckPods: force-deleted Longhorn-PVC pods on departing node to release attachments",
			"node", nodeName, "pods", evicted, "machine", machine.Name)
		r.emitEvent(machine, corev1.EventTypeNormal, "LonghornStuckPodEvicted",
			fmt.Sprintf("Departing node %s: force-deleted Longhorn-PVC pods %v to release volume attachments (WaitingForVolumeDetach)", nodeName, evicted))
	}
}

// listPodsWithRetry lists pods on the departing node with a short bounded
// retry: first via the spec.nodeName field selector, falling back to a full
// list + filter. Tolerates the transient API churn seen mid-roll (E15).
func (r *LonghornEvictionReconciler) listPodsWithRetry(ctx context.Context, wlClient client.Client, nodeName string) (*corev1.PodList, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second): // 2s, 4s, 6s
			}
		}
		podList := &corev1.PodList{}
		if err := wlClient.List(ctx, podList, client.InNamespace(""), client.MatchingFields{"spec.nodeName": nodeName}); err == nil {
			return podList, nil
		} else {
			lastErr = err
		}
		// Fallback: full list + filter (field selector may be unsupported).
		podList = &corev1.PodList{}
		if err := wlClient.List(ctx, podList); err == nil {
			return podList, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

// isDaemonSetPod reports whether the pod is managed by a DaemonSet (by owner ref).
func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// podHasAnyPVC reports whether any volume in the pod references a PVC. This
// is only a cheap pre-filter; whether the claim is Longhorn-backed is
// confirmed positively by podLonghornVolumeState.
func podHasAnyPVC(pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// podLonghornVolumeState reports whether the pod consumes at least one
// Longhorn volume, and whether any of those volumes is currently faulted.
// Claims that do not resolve to a Longhorn Volume CR do not count as
// Longhorn. Transient lookup errors are returned as errors so the caller
// skips the pod instead of evicting on unverified volume state.
func (r *LonghornEvictionReconciler) podLonghornVolumeState(ctx context.Context, wlClient client.Client, pod *corev1.Pod) (hasLonghorn, faulted bool, err error) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: v.PersistentVolumeClaim.ClaimName, Namespace: pod.Namespace}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				continue // PVC gone — no attachment to release
			}
			return false, false, fmt.Errorf("getting PVC %s/%s: %w", pod.Namespace, v.PersistentVolumeClaim.ClaimName, err)
		}
		if pvc.Spec.VolumeName == "" {
			continue // not bound — no attachment to release
		}
		vol := &longhornv1beta2.Volume{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName, Namespace: r.LonghornNS}, vol); err != nil {
			if apierrors.IsNotFound(err) {
				continue // not a Longhorn volume
			}
			return false, false, fmt.Errorf("getting Longhorn volume %s: %w", pvc.Spec.VolumeName, err)
		}
		hasLonghorn = true
		if vol.Status.Robustness == longhornv1beta2.VolumeRobustnessFaulted {
			faulted = true
		}
	}
	return hasLonghorn, faulted, nil
}

// workloadScheme is a package-level scheme for workload cluster clients.
var workloadScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = longhornv1beta2.AddToScheme(s)
	return s
}()

// buildWorkloadClusterClient constructs a controller-runtime client for the
// workload cluster identified by clusterName/namespace by reading the
// <clusterName>-kubeconfig Secret through an UNCACHED reader. Passing the
// manager's cached client here would lazily create a Secret informer that
// lists and watches every Secret in the management cluster — widening the RBAC
// surface and keeping every workload cluster's admin credential resident in
// controller memory.
func buildWorkloadClusterClient(ctx context.Context, secretReader client.Reader, clusterName, namespace string) (client.Client, error) {
	secretName := clusterName + "-kubeconfig"
	secret := &corev1.Secret{}
	if err := secretReader.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		return nil, fmt.Errorf("fetching kubeconfig secret %s/%s: %w", namespace, secretName, err)
	}
	kubeconfigBytes, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %s/%s missing 'value' key", namespace, secretName)
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("building rest.Config from kubeconfig for cluster %s: %w", clusterName, err)
	}
	wlClient, err := client.New(restConfig, client.Options{Scheme: workloadScheme})
	if err != nil {
		return nil, fmt.Errorf("creating workload cluster client for %s: %w", clusterName, err)
	}
	return wlClient, nil
}

func (r *LonghornEvictionReconciler) emitEvent(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, eventType, reason, "%s", message)
	}
}
