package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
)

func gcScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := longhornv1beta2.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func lhNode(name string, deleting bool, finalizers ...string) *longhornv1beta2.Node {
	n := &longhornv1beta2.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "longhorn-system",
			Finalizers: finalizers,
		},
	}
	if deleting {
		now := metav1.Now()
		n.DeletionTimestamp = &now
	}
	return n
}

func k8sNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// Invariant 1: a Longhorn node whose k8s Node still exists (even NotReady)
// must never be touched.
func TestGCSkipsLiveK8sNodes(t *testing.T) {
	ctx := context.Background()
	s := gcScheme(t)

	liveReady := k8sNode("worker-live")
	liveNotReady := k8sNode("worker-notready")
	liveNotReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}

	lhLive := lhNode("worker-live", false, LonghornFinalizer)
	lhNotReady := lhNode("worker-notready", false, LonghornFinalizer)
	lhGone := lhNode("worker-gone", false, LonghornFinalizer)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(liveReady, liveNotReady, lhLive, lhNotReady, lhGone).Build()
	r := &LonghornNodeGCReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, err := r.gcOrphanedNodes(ctx, c)
	if err != nil {
		t.Fatalf("gcOrphanedNodes: %v", err)
	}
	if len(removed) != 1 || removed[0] != "worker-gone" {
		t.Fatalf("expected only worker-gone removed, got %v", removed)
	}

	for _, name := range []string{"worker-live", "worker-notready"} {
		var got longhornv1beta2.Node
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "longhorn-system"}, &got); err != nil {
			t.Fatalf("live node %s was deleted: %v", name, err)
		}
		if !sets.New(got.Finalizers...).Has(LonghornFinalizer) {
			t.Fatalf("live node %s finalizer was removed", name)
		}
		if got.DeletionTimestamp != nil {
			t.Fatalf("live node %s got a deletionTimestamp", name)
		}
	}
}

// Invariant 2: a stuck-deleting CR (deletionTimestamp + longhorn.io
// finalizer) for a gone k8s node gets its finalizer removed so the pending
// deletion can complete.
func TestGCUnsticksStuckDeletingNode(t *testing.T) {
	ctx := context.Background()
	s := gcScheme(t)

	stuck := lhNode("worker-stuck", true, LonghornFinalizer)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(stuck).Build()
	r := &LonghornNodeGCReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, err := r.gcOrphanedNodes(ctx, c)
	if err != nil {
		t.Fatalf("gcOrphanedNodes: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed, got %v", removed)
	}

	var got longhornv1beta2.Node
	err = c.Get(ctx, types.NamespacedName{Name: "worker-stuck", Namespace: "longhorn-system"}, &got)
	if err == nil {
		// Still present is acceptable mid-flight ONLY if finalizers are now empty.
		if len(got.Finalizers) != 0 {
			t.Fatalf("stuck node finalizers not removed: %v", got.Finalizers)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected get error: %v", err)
	}
}

// Invariant 3: a non-deleting CR for a gone k8s node is deleted (documented
// remediation, SUSE KB).
func TestGCDeletesOrphanNode(t *testing.T) {
	ctx := context.Background()
	s := gcScheme(t)

	orphan := lhNode("worker-orphan", false, LonghornFinalizer)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(orphan).Build()
	r := &LonghornNodeGCReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, err := r.gcOrphanedNodes(ctx, c)
	if err != nil {
		t.Fatalf("gcOrphanedNodes: %v", err)
	}
	if len(removed) != 1 || removed[0] != "worker-orphan" {
		t.Fatalf("expected worker-orphan removed, got %v", removed)
	}

	var got longhornv1beta2.Node
	if err := c.Get(ctx, types.NamespacedName{Name: "worker-orphan", Namespace: "longhorn-system"}, &got); err == nil {
		if len(got.Finalizers) != 0 {
			t.Fatalf("orphan node still has finalizers after GC: %v", got.Finalizers)
		}
	}
	// Deletion is fine; finalizer removal guarantees it can complete.
}
