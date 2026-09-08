package controller

import (
	"context"
	"testing"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// evScheme builds a scheme with Longhorn types for eviction tests.
func evScheme(t *testing.T) *runtime.Scheme {
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

func mkVolume(name string, want int, robustness string) *longhornv1beta2.Volume {
	return &longhornv1beta2.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "longhorn-system"},
		Spec:       longhornv1beta2.VolumeSpec{NumberOfReplicas: want},
		Status:     longhornv1beta2.VolumeStatus{Robustness: longhornv1beta2.VolumeRobustness(robustness)},
	}
}

func mkReplica(vol, node, state, failedAt string) *longhornv1beta2.Replica {
	return &longhornv1beta2.Replica{
		ObjectMeta: metav1.ObjectMeta{Name: vol + "-r-" + node, Namespace: "longhorn-system"},
		Spec: longhornv1beta2.ReplicaSpec{
			NodeID:     node,
			EngineName: vol + "-e-0",
			FailedAt:   failedAt,
		},
		Status: longhornv1beta2.ReplicaStatus{
			InstanceStatus: longhornv1beta2.InstanceStatus{CurrentState: longhornv1beta2.InstanceState(state)},
		},
	}
}

// Early release must fire when eviction is drained, the volume is degraded
// (want-1 running replicas on survivors), and NOT faulted.
func TestEarlyReleaseSafeOnDegraded(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		mkVolume("vol-a", 3, "degraded"),
		mkReplica("vol-a", "node-live-1", "running", ""),
		mkReplica("vol-a", "node-live-2", "running", ""),
		mkReplica("vol-a", "node-departing", "stopped", ""),
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	safe, _, err := r.earlyReleaseSafe(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !safe {
		t.Fatal("expected early-release SAFE for degraded volume with want-1 live replicas")
	}
}

// Early release must HOLD when the volume is faulted.
func TestEarlyReleaseHoldsOnFaulted(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		mkVolume("vol-f", 3, "faulted"),
		mkReplica("vol-f", "node-live-1", "running", ""),
		mkReplica("vol-f", "node-live-2", "running", ""),
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	safe, _, err := r.earlyReleaseSafe(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if safe {
		t.Fatal("expected early-release HOLD on faulted volume")
	}
}

// Early release must HOLD when redundancy is worse than want-1 (e.g. only 1
// of 3 replicas remains) — data at risk, wait for rebuild or timeout.
func TestEarlyReleaseHoldsOnInsufficientReplicas(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		mkVolume("vol-i", 3, "degraded"),
		mkReplica("vol-i", "node-live-1", "running", ""),
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	safe, _, err := r.earlyReleaseSafe(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if safe {
		t.Fatal("expected early-release HOLD when replicas < want-1")
	}
}

// A volume that already has its full replica count is "complete" — the early
// release path must NOT fire (the normal rebuild-complete gate handles it).
func TestEarlyReleaseNotNeededWhenComplete(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		mkVolume("vol-c", 3, "healthy"),
		mkReplica("vol-c", "node-live-1", "running", ""),
		mkReplica("vol-c", "node-live-2", "running", ""),
		mkReplica("vol-c", "node-live-3", "running", ""),
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	safe, _, err := r.earlyReleaseSafe(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if safe {
		t.Fatal("expected early-release NOT to fire when all volumes are complete")
	}
}
