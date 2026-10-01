package controller

import (
	"context"
	"testing"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Regression tests for CRIT-1a (found executing S2 of the validation plan on
// lhcc-roll1, 2026-10-01): the last-live-replica guard counted ANY running
// replica as a surviving copy — including a replacement created seconds
// earlier by Longhorn's own eviction replenishment, which is running but holds
// no data until its rebuild completes. The guard must count only replicas with
// Spec.HealthyAt set (cleared before a rebuild, set when the replica goes
// read/write).

// midRebuildReplica returns a running replica that has NOT completed a rebuild
// (HealthyAt empty).
func midRebuildReplica(vol, node string) *longhornv1beta2.Replica {
	r := mkReplica(vol, node, "running", "")
	r.Spec.HealthyAt = ""
	return r
}

// A want=1 volume whose only surviving replica is a just-created replacement
// (running, HealthyAt empty) must NOT have its doomed source replica deleted:
// the replacement holds no data yet, so the source is the only copy.
func TestCleanupRefusesWhileReplacementMidRebuild(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-live-1"}},
		mkVolume("vol-r1", 1, "degraded"),
		mkReplica("vol-r1", "node-departing", "running", ""), // the source; holds the data
		midRebuildReplica("vol-r1", "node-live-1"),           // replenished, still syncing
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, _, err := r.cleanupBadReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("refusal must hold while the replacement holds no data; deleted: %v", removed)
	}
}

// Once the replacement has completed its rebuild (HealthyAt set), the doomed
// source may be deleted — the volume's data now lives on the survivor.
func TestCleanupAllowsAfterReplacementHoldsData(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-live-1"}},
		mkVolume("vol-r1b", 1, "degraded"),
		mkReplica("vol-r1b", "node-departing", "running", ""),
		mkReplica("vol-r1b", "node-live-1", "running", ""), // rebuild complete
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, _, err := r.cleanupBadReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected the doomed replica to be deleted once a survivor holds data; removed=%v", removed)
	}
}

// S1 semantics preserved: a multi-replica volume with data-holding survivors
// gets its doomed replica deleted (prompt rebuild unblock).
func TestCleanupDeletesDoomedWithHealthySurvivors(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-live-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-live-2"}},
		mkVolume("vol-s1", 3, "degraded"),
		mkReplica("vol-s1", "node-departing", "running", ""),
		mkReplica("vol-s1", "node-live-1", "running", ""),
		mkReplica("vol-s1", "node-live-2", "running", ""),
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, _, err := r.cleanupBadReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected doomed replica deletion with 2 data-holding survivors; removed=%v", removed)
	}
}

// Early release must HOLD for a want=1 volume while its only non-doomed
// replica is still mid-rebuild: max(want-1, 1) == 1 data-holding copy required,
// and the unsynced replacement does not count.
func TestEarlyReleaseHoldsSingleReplicaMidRebuild(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		mkVolume("vol-w1", 1, "degraded"),
		mkReplica("vol-w1", "node-departing", "running", ""), // source, doomed
		midRebuildReplica("vol-w1", "node-live-1"),           // replenished, syncing
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	safe, _, err := r.earlyReleaseSafe(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if safe {
		t.Fatal("early release must hold while a want=1 volume has no data-holding replica")
	}
}

// A mid-rebuild replacement must not satisfy the cleanup guard even on a
// multi-replica volume whose other survivors are gone — the guard's count is
// over data-holding replicas only.
func TestCleanupRefusesWhenOnlyMidRebuildSurvivorRemains(t *testing.T) {
	s := evScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-live-1"}},
		mkVolume("vol-w3", 3, "degraded"),
		mkReplica("vol-w3", "node-departing", "running", ""),
		midRebuildReplica("vol-w3", "node-live-1"),
	).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	removed, _, err := r.cleanupBadReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("refusal must hold when the only survivor is mid-rebuild; deleted: %v", removed)
	}
}