package controller

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
)

// LonghornFinalizer is the finalizer longhorn-manager places on Node CRs.
const LonghornFinalizer = "longhorn.io"

// LonghornNodeGCReconciler removes orphaned nodes.longhorn.io CRs whose
// Kubernetes Node is gone.
//
// Rationale (longhorn/longhorn#6487, closed wontfix): longhorn-manager's
// kubernetes_node_controller is supposed to delete the Node CR when the k8s
// Node is deleted, but if the node went down before deletion the finalizer is
// never removed and the CR is stuck deleting forever. SUSE's documented
// remediation is to delete the nodes.longhorn.io CR manually.
//
// Safety rules:
//   - The k8s Node list is the SOURCE OF TRUTH. A Longhorn node is only
//     touched when its k8s Node no longer exists in the workload cluster.
//   - A stuck-deleting CR (deletionTimestamp set, longhorn.io finalizer held)
//     gets the finalizer removed, which lets the pending deletion complete.
//   - A non-deleting CR for a gone node gets deleted outright (documented
//     remediation). Replicas on it are already stopped/unreachable.
//   - Nodes whose k8s Node exists (even NotReady) are NEVER touched here.
//
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
type LonghornNodeGCReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	GCInterval   time.Duration
	LonghornNS   string
	workloadClients map[string]client.Client // "namespace/clusterName" -> client
}

// SetupWithManager registers the GC as a Machine-watching controller plus a
// periodic ticker, so orphans are swept even with no events flowing.
func (r *LonghornNodeGCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.workloadClients = map[string]client.Client{}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}).
		Complete(r); err != nil {
		return err
	}

	// Periodic sweep so orphans are cleaned even if no Cluster events fire.
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		t := time.NewTicker(r.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				_, _ = r.Reconcile(ctx, ctrl.Request{})
			}
		}
	}))
}

// Reconcile runs a GC pass across all CAPI clusters.
func (r *LonghornNodeGCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("gc", "longhorn-node")

	clusters := &clusterv1.ClusterList{}
	if err := r.List(ctx, clusters); err != nil {
		return ctrl.Result{}, err
	}

	for i := range clusters.Items {
		c := &clusters.Items[i]
		key := c.Namespace + "/" + c.Name
		wlClient, err := r.workloadClient(ctx, key, c.Name, c.Namespace)
		if err != nil {
			// Cluster may not have a kubeconfig yet; try again next pass.
			logger.V(1).Info("skipping cluster: no workload client", "cluster", key, "err", err.Error())
			continue
		}
		removed, err := r.gcOrphanedNodes(ctx, wlClient)
		if err != nil {
			logger.Error(err, "GC pass failed", "cluster", key)
			continue
		}
		if len(removed) > 0 {
			logger.Info("removed orphaned Longhorn nodes", "cluster", key, "nodes", removed)
			r.emitGCEvent(c, "removed orphaned Longhorn nodes: "+strings.Join(removed, ","))
		}
	}
	return ctrl.Result{}, nil
}

// workloadClient returns a cached workload-cluster client for the key.
func (r *LonghornNodeGCReconciler) workloadClient(ctx context.Context, key, clusterName, namespace string) (client.Client, error) {
	if c, ok := r.workloadClients[key]; ok {
		return c, nil
	}
	c, err := buildWorkloadClusterClient(ctx, r.Client, clusterName, namespace)
	if err != nil {
		return nil, err
	}
	r.workloadClients[key] = c
	return c, nil
}

// gcOrphanedNodes removes Longhorn Node CRs whose k8s Node is gone.
func (r *LonghornNodeGCReconciler) gcOrphanedNodes(ctx context.Context, wlClient client.Client) ([]string, error) {
	lhNodes := &longhornv1beta2.NodeList{}
	if err := wlClient.List(ctx, lhNodes, client.InNamespace(r.LonghornNS)); err != nil {
		// Clusters without Longhorn (e.g. k3s tooling clusters) fail the list
		// with a REST-mapping "no matches for kind" error. That is not an
		// error for the GC — skip them quietly.
		if isNoMatchErr(err) {
			return nil, nil
		}
		return nil, err
	}

	k8sNodes := &corev1.NodeList{}
	if err := wlClient.List(ctx, k8sNodes); err != nil {
		return nil, err
	}
	live := sets.New[string]()
	for _, n := range k8sNodes.Items {
		live.Insert(n.Name)
	}

	var removed []string
	for i := range lhNodes.Items {
		ln := &lhNodes.Items[i]
		if live.Has(ln.Name) {
			continue // k8s node still exists — never touch (source of truth).
		}

		// k8s node is gone. If the CR is stuck deleting, unstick it; if it is
		// not yet deleting, delete it. Either way the finalizer must go.
		patch := client.MergeFrom(ln.DeepCopy())
		finalizers := sets.New[string](ln.Finalizers...)
		finalizers.Delete(LonghornFinalizer)
		ln.Finalizers = sets.List(finalizers)

		if ln.DeletionTimestamp.IsZero() {
			if err := wlClient.Delete(ctx, ln); err != nil && !apierrors.IsNotFound(err) {
				return removed, err
			}
		}
		if err := wlClient.Patch(ctx, ln, patch); err != nil && !apierrors.IsNotFound(err) {
			return removed, err
		}
		removed = append(removed, ln.Name)
	}
	return removed, nil
}

func (r *LonghornNodeGCReconciler) emitGCEvent(obj runtime.Object, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, corev1.EventTypeNormal, "LonghornNodeGC", message)
	}
}

// isNoMatchErr reports whether err is a REST-mapping "no matches" error (i.e.
// the CRD is absent in the target cluster). Matches both the classic
// "no matches for kind X in version Y" phrasing and the discovery variant
// "no matches for group/version, Resource=" / "no matches for group/version, Resource=...".
func isNoMatchErr(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*meta.NoKindMatchError); ok {
		return true
	}
	if _, ok := err.(*meta.NoResourceMatchError); ok {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "no matches for longhorn.io") ||
		strings.Contains(msg, "unable to retrieve the complete list of server APIs: longhorn.io")
}
