package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
const HookReleasedAnnotation = "longhorn-capi.sneezycats.io/hook-released"

// LonghornEvictionReconciler reconciles CAPI Machine objects to ensure
// Longhorn replicas are fully evicted before the Machine is terminated.
//
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type LonghornEvictionReconciler struct {
	client.Client
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
			if wlClient, err := buildWorkloadClusterClient(ctx, r.Client, machine.Spec.ClusterName, machine.Namespace); err == nil {
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
	wlClient, err := buildWorkloadClusterClient(ctx, r.Client, clusterName, machine.Namespace)
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
	delNodes, err := r.deletingNodeNames(ctx)
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
		replicasDrained, err := r.nodeReplicasDrained(ctx, wlClient, nodeName, delNodes)
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
func isEvictionComplete(node *longhornv1beta2.Node) bool {
	if node.Status.DiskStatus == nil || len(node.Status.DiskStatus) == 0 {
		return true
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
func (r *LonghornEvictionReconciler) nodeReplicasDrained(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (bool, error) {
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

// deletingNodeNames returns the set of node names that CAPI is currently deleting,
// derived from Machines in the management cluster that have a deletionTimestamp AND
// a non-nil status.nodeRef. This is a cluster-wide view so that replicas on ANY
// doomed node are caught, not just the Machine that triggered this reconcile.
func (r *LonghornEvictionReconciler) deletingNodeNames(ctx context.Context) (map[string]bool, error) {
	machineList := &clusterv1.MachineList{}
	if err := r.List(ctx, machineList); err != nil {
		return nil, fmt.Errorf("listing machines to find deleting nodes: %w", err)
	}
	out := map[string]bool{}
	for i := range machineList.Items {
		m := &machineList.Items[i]
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
//   (a) any replica (running or stopped) on a node CAPI is deleting (doomed), and
//   (b) any STOPPED replica whose node is no longer a live, ready Longhorn node
//       (gone/NotReady) — observed as the stale `stopped` replica that kept a volume
//       at 3 "healthy" replicas while one never left a doomed node and the rebuild
//       stalled (longhorn-maintenance-behavior.md E6).
// Returns (removedNames, affectedVolumes). Runs regardless of eviction drain state so
// the rebuild is unblocked promptly rather than waiting out the eviction timeout.
func (r *LonghornEvictionReconciler) cleanupBadReplicas(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (removed []string, affected []string, err error) {
	// Live, scheduling-eligible Longhorn node hostnames (targets for a rebuild).
	liveNodes, err := r.liveNodeNames(ctx, wlClient)
	if err != nil {
		return nil, nil, fmt.Errorf("listing live Longhorn nodes: %w", err)
	}

	replicaList := &longhornv1beta2.ReplicaList{}
	if err := wlClient.List(ctx, replicaList, client.InNamespace(r.LonghornNS)); err != nil {
		return nil, nil, fmt.Errorf("listing replicas in %s: %w", r.LonghornNS, err)
	}

	affectedVolumes := map[string]bool{}
	for i := range replicaList.Items {
		rep := &replicaList.Items[i]
		nodeID := rep.Spec.NodeID
		volName := volumeNameFromReplica(rep)
		if volName != "" {
			affectedVolumes[volName] = true
		}
		doomed := nodeID == departingNode || deletingNodes[nodeID]
		onGoneNode := nodeID != "" && !liveNodes[nodeID] && !rep.Spec.Active
		if !doomed && !onGoneNode {
			continue
		}
		l := log.FromContext(ctx)
		l.Info("Deleting bad replica to force a clean rebuild",
			"replica", rep.Name, "node", nodeID, "volume", volName, "doomed", doomed, "onGoneNode", onGoneNode)
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
// A faulted volume or a replica deficit beyond want-1 still holds the hook.
func (r *LonghornEvictionReconciler) earlyReleaseSafe(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (bool, []string, error) {
	names, allComplete, degradedSafe, err := r.volumeReplicaState(ctx, wlClient, departingNode, deletingNodes)
	if err != nil || allComplete {
		return false, names, err
	}
	return degradedSafe, names, nil
}

// volumeReplicaState returns the sorted affected-volume names, whether ALL
// affected volumes already meet their full replica count on live nodes
// (allComplete), and whether they are all degraded-but-safe (>= want-1 live
// replicas each and none faulted — degradedSafe). Faulted forces degradedSafe=false.
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
		if doomed || !isReplicaRunning(rep) {
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
		if liveByVolume[volName] < want-1 {
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
// Longhorn PVC, and (c) whose volume is NOT faulted (replicas healthy on
// surviving nodes). The pod reschedules to a live node and the volume detaches.
// DaemonSet pods are never touched (they belong on every node by design).
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
			if err := wlClient.Patch(ctx, k8sNode, patch); err == nil {
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
		if !podHasLonghornPVC(pod) {
			continue
		}
		// Safety: skip if any of the pod's Longhorn volumes is faulted.
		faulted, err := r.podVolumesFaulted(ctx, wlClient, pod)
		if err != nil {
			l.Error(err, "EvictStuckPods: failed to evaluate pod volumes — skipping pod",
				"pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		if faulted {
			l.Info("EvictStuckPods: pod holds a FAULTED Longhorn volume — not evicting (safety)",
				"pod", pod.Name, "namespace", pod.Namespace, "node", nodeName)
			continue
		}
		if err := wlClient.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
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

// podHasLonghornPVC reports whether any volume in the pod references a PVC.
// (Provisioner confirmation happens in podVolumesFaulted via the Volume CRs.)
func podHasLonghornPVC(pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// podVolumesFaulted reports whether any Longhorn-backed volume of the pod is
// currently faulted. Volumes not provisioned by Longhorn are ignored.
func (r *LonghornEvictionReconciler) podVolumesFaulted(ctx context.Context, wlClient client.Client, pod *corev1.Pod) (bool, error) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: v.PersistentVolumeClaim.ClaimName, Namespace: pod.Namespace}, pvc); err != nil {
			continue // cannot resolve — treat as not-faulted (best effort)
		}
		if pvc.Spec.VolumeName == "" {
			continue
		}
		vol := &longhornv1beta2.Volume{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName, Namespace: r.LonghornNS}, vol); err != nil {
			continue // not a Longhorn volume (or gone) — ignore
		}
		if vol.Status.Robustness == longhornv1beta2.VolumeRobustnessFaulted {
			return true, nil
		}
	}
	return false, nil
}

// workloadScheme is a package-level scheme for workload cluster clients.
var workloadScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = longhornv1beta2.AddToScheme(s)
	return s
}()

// buildWorkloadClusterClient constructs a controller-runtime client for the workload cluster
// identified by clusterName/namespace by reading the <clusterName>-kubeconfig Secret.
func buildWorkloadClusterClient(ctx context.Context, mgmtClient client.Client, clusterName, namespace string) (client.Client, error) {
	secretName := clusterName + "-kubeconfig"
	secret := &corev1.Secret{}
	if err := mgmtClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
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

// Ensure metatime import is retained for go mod tidy.
var _ = metav1.Now
