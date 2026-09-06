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
	wlClient, err := r.buildWorkloadClusterClient(ctx, clusterName, machine.Namespace)
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

	// 7. Check completion.
	if !isEvictionComplete(lhNode) {
		l.Info("Longhorn eviction in progress — requeueing", "machine", machine.Name, "node", nodeName, "elapsed", elapsed.Truncate(time.Second))
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}

	// 7b. Rebuild-completion gate: eviction may have drained the node, but the
	// replicas that lived here must be REBUILT onto surviving nodes before CAPI
	// proceeds. Releasing the hook just on drain (the KB's classic approach) lets
	// the replacement node come up with zero replicas and a Degraded/stuck volume.
	// Wait until every volume that lost a replica here reaches spec.numberOfReplicas
	// with running replicas on surviving nodes (and robustness is not degraded
	// solely due to the missing replica).
	rebuildComplete, affected, err := r.rebuildComplete(ctx, wlClient, nodeName)
	if err != nil {
		l.Error(err, "Failed to evaluate replica rebuild state — requeueing", "machine", machine.Name, "node", nodeName)
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

// rebuildComplete reports whether every volume that lost a replica on the departing
// node has rebuilt to spec.numberOfReplicas running replicas on SURVIVING nodes.
// It returns (complete, listOfAffectedVolumes, error). If no replicas referenced the
// departing node, it returns (true, nil, nil) — nothing to rebuild.
//
// This closes the gap where eviction drains the node but Longhorn's rebuild of those
// replicas onto remaining nodes is stalled (see longhorn-maintenance-behavior.md
// E6/E7): releasing the pre-terminate hook on eviction alone lets CAPI replace the
// node while the new node comes up with zero/a-degraded replica set.
func (r *LonghornEvictionReconciler) rebuildComplete(ctx context.Context, wlClient client.Client, departingNode string) (bool, []string, error) {
	// 1. List replicas to find which volumes had a replica on the departing node.
	replicaList := &longhornv1beta2.ReplicaList{}
	if err := wlClient.List(ctx, replicaList, client.InNamespace(r.LonghornNS)); err != nil {
		return false, nil, fmt.Errorf("listing replicas in %s: %w", r.LonghornNS, err)
	}

	affectedVolumes := map[string]bool{}
	for _, rep := range replicaList.Items {
		if rep.Spec.NodeID == departingNode && rep.Spec.Active {
			if volName := volumeNameFromReplica(&rep); volName != "" {
				affectedVolumes[volName] = true
			}
		}
	}
	if len(affectedVolumes) == 0 {
		return true, nil, nil
	}

	// Sort affected volumes for deterministic output.
	names := make([]string, 0, len(affectedVolumes))
	for n := range affectedVolumes {
		names = append(names, n)
	}
	sort.Strings(names)

	// 2. Group replicas by volume for the running-on-surviving-node count.
	liveByVolume := map[string]int{}
	for _, rep := range replicaList.Items {
		if !rep.Spec.Active || rep.Spec.NodeID == departingNode {
			continue
		}
		// Only count replicas that are actually running (not stopped/erroring).
		if !isReplicaRunning(rep) {
			continue
		}
		if volName := volumeNameFromReplica(&rep); volName != "" {
			liveByVolume[volName]++
		}
	}

	// 3. For each affected volume, require live >= spec.numberOfReplicas and not
	// degraded-due-to-missing-replica.
	for _, volName := range names {
		vol := &longhornv1beta2.Volume{}
		if err := wlClient.Get(ctx, types.NamespacedName{Name: volName, Namespace: r.LonghornNS}, vol); err != nil {
			// Volume gone (deleted) — nothing to rebuild.
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, names, fmt.Errorf("getting volume %s: %w", volName, err)
		}
		want := vol.Spec.NumberOfReplicas
		if want <= 0 {
			want = 1
		}
		if liveByVolume[volName] < want {
			return false, names, nil
		}
		// If robustness is degraded, it may still be mid-rebuild. Only treat as
		// incomplete if it is faulted (data-loss) — degraded with enough live
		// replicas is a rebuild-in-progress we allow to latch onto completion
		// once live==want. To be safe we require not-faulted.
		if vol.Status.Robustness == longhornv1beta2.VolumeRobustnessFaulted {
			return false, names, nil
		}
	}

	return true, names, nil
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
func (r *LonghornEvictionReconciler) buildWorkloadClusterClient(ctx context.Context, clusterName, namespace string) (client.Client, error) {
	secretName := clusterName + "-kubeconfig"
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
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
