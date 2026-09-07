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
		return ctrl.Result{}, nil
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

	// Now check eviction drain state.
	if !isEvictionComplete(lhNode) {
		l.Info("Longhorn eviction in progress — requeueing", "machine", machine.Name, "node", nodeName, "elapsed", elapsed.Truncate(time.Second))
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	// Rebuild-completion gate: eviction drained, but the replicas must be REBUILT onto
	// surviving nodes before CAPI proceeds. Wait until every volume has
	// spec.numberOfReplicas running replicas on NON-deleting nodes.
	rebuildComplete, err := r.volumesRebuilt(ctx, wlClient, nodeName, delNodes)
	if err != nil {
		l.Error(err, "Failed to evaluate rebuild state — requeueing", "machine", machine.Name, "node", nodeName)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	if !rebuildComplete {
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
func (r *LonghornEvictionReconciler) volumesRebuilt(ctx context.Context, wlClient client.Client, departingNode string, deletingNodes map[string]bool) (complete bool, err error) {
	replicaList := &longhornv1beta2.ReplicaList{}
	if err := wlClient.List(ctx, replicaList, client.InNamespace(r.LonghornNS)); err != nil {
		return false, fmt.Errorf("listing replicas in %s: %w", r.LonghornNS, err)
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

	names := make([]string, 0, len(affectedVolumes))
	for n := range affectedVolumes {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, volName := range names {
		vol := &longhornv1beta2.Volume{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: volName, Namespace: r.LonghornNS}, vol); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("getting volume %s: %w", volName, err)
		}
		want := vol.Spec.NumberOfReplicas
		if want <= 0 {
			want = 1
		}
		if liveByVolume[volName] < want {
			return false, nil
		}
		if vol.Status.Robustness == longhornv1beta2.VolumeRobustnessFaulted {
			return false, nil
		}
	}
	return true, nil
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
