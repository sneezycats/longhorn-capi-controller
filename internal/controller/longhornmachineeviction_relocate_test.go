package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Regression tests for CRIT-1b (found executing the 6.0→6.1 upgrade roll on
// lhcc-roll1, 2026-10-01): in the machine-set roll flow the infra VM is
// deleted hook-blind seconds after the machine deletion starts, and Longhorn
// eviction does not replenish a last replica whose volume is attached to a
// DIFFERENT node — the single-replica volume was lost. The fix: relocate the
// data immediately (raise spec.numberOfReplicas so Longhorn's standard
// scheduling rebuilds a copy on a surviving node), uncordon the node if the
// source replica failed anyway (salvage from the intact disk), and restore the
// original count on release.

// evRelocateScheme: evScheme plus the Cluster-API Machine type (the relocation
// patch writes a Machine annotation through the same client).
func evRelocateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := evScheme(t)
	if err := clusterv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// mkDelMachine: mkMachine with a finalizer so the fake client accepts a
// machine carrying a deletionTimestamp (refused otherwise).
func mkDelMachine(nodeName string) *clusterv1.Machine {
	m := mkMachine(nodeName)
	m.Finalizers = append(m.Finalizers, "cluster.x-k8s.io/machine")
	return m
}

// getVolAfter re-fetches a volume from the fake client: WithObjects deep-copies
// its arguments, so assertions must read the client's copy, never the local one.
func getVolAfter(t *testing.T, c client.Client, name string) *longhornv1beta2.Volume {
	t.Helper()
	vol := &longhornv1beta2.Volume{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "longhorn-system"}, vol); err != nil {
		t.Fatal(err)
	}
	return vol
}

// TestRelocateRaisesWantForLastReplicaVolume: a want=1 volume whose only
// data-holding replica sits on the departing node gets its replica count
// raised (1→2) and the original recorded on the Machine annotation.
func TestRelocateRaisesWantForLastReplicaVolume(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	src := mkReplica("pvc-x", "node-departing", "running", "")
	machine := mkDelMachine("node-departing")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	relocated, err := r.relocateLastReplicas(context.Background(), c, machine, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !relocated {
		t.Fatal("expected relocation to fire for a last-replica volume")
	}
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 2 {
		t.Fatalf("expected want raised 1→2, got %d", vol.Spec.NumberOfReplicas)
	}
	ann := machine.Annotations[ReplicaScaleOrigAnnotation]
	if !strings.Contains(ann, "pvc-x=1") {
		t.Fatalf("expected original want recorded on the machine, got %q", ann)
	}
}

// TestRelocateSkipsWhenSurvivorHoldsData: with a data-holding replica on a
// surviving node there is nothing to relocate — the count is untouched and no
// annotation is recorded.
func TestRelocateSkipsWhenSurvivorHoldsData(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	src := mkReplica("pvc-x", "node-departing", "running", "")
	survivor := mkReplica("pvc-x", "node-alive", "running", "")
	machine := mkDelMachine("node-departing")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src, survivor, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	relocated, err := r.relocateLastReplicas(context.Background(), c, machine, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if relocated {
		t.Fatal("expected no relocation when a survivor holds the data")
	}
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 1 {
		t.Fatalf("expected want untouched, got %d", vol.Spec.NumberOfReplicas)
	}
	if machine.Annotations[ReplicaScaleOrigAnnotation] != "" {
		t.Fatalf("expected no relocation annotation, got %q", machine.Annotations[ReplicaScaleOrigAnnotation])
	}
}

// TestRelocateIdempotentViaAnnotation: a second pass (next poll) must not
// double-raise; the annotation is the idempotence guard.
func TestRelocateIdempotentViaAnnotation(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	src := mkReplica("pvc-x", "node-departing", "running", "")
	machine := mkDelMachine("node-departing")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}
	ctx := context.Background()

	if _, err := r.relocateLastReplicas(ctx, c, machine, "node-departing", nil); err != nil {
		t.Fatal(err)
	}
	relocated, err := r.relocateLastReplicas(ctx, c, machine, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if relocated {
		t.Fatal("second pass must be a no-op (idempotent)")
	}
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 2 {
		t.Fatalf("expected want to stay at 2 after repeat pass, got %d", vol.Spec.NumberOfReplicas)
	}
}

// TestRelocateNoOpWhenDoomedReplicaHoldsNoData: a stopped (data-less) doomed
// replica is garbage — no relocation should fire for it.
func TestRelocateNoOpWhenDoomedReplicaHoldsNoData(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	garbage := mkReplica("pvc-x", "node-departing", "stopped", "")
	garbage.Spec.HealthyAt = ""
	machine := mkDelMachine("node-departing")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, garbage, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	relocated, err := r.relocateLastReplicas(context.Background(), c, machine, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if relocated {
		t.Fatal("expected no relocation for a data-less doomed replica")
	}
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 1 {
		t.Fatalf("expected want untouched, got %d", vol.Spec.NumberOfReplicas)
	}
}

// TestRelocateSkipsWhenVolumeGone: a deleted volume's replica is not a
// relocation candidate.
func TestRelocateSkipsWhenVolumeGone(t *testing.T) {
	s := evRelocateScheme(t)
	src := mkReplica("pvc-gone", "node-departing", "running", "")
	machine := mkDelMachine("node-departing")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(src, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	if _, err := r.relocateLastReplicas(context.Background(), c, machine, "node-departing", nil); err != nil {
		t.Fatal(err)
	}
	if machine.Annotations[ReplicaScaleOrigAnnotation] != "" {
		t.Fatal("expected no annotation for a missing volume")
	}
}

// TestRestoreReplicaWantsRestoresCount: the release path restores the recorded
// original count.
func TestRestoreReplicaWantsRestoresCount(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 2, "healthy")
	machine := mkDelMachine("node-departing")
	machine.Annotations[ReplicaScaleOrigAnnotation] = "pvc-x=1"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	r.restoreReplicaWants(context.Background(), c, machine)
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 1 {
		t.Fatalf("expected want restored to 1, got %d", vol.Spec.NumberOfReplicas)
	}
}

// TestRestoreSkipsWhenAlreadyAtOriginal: idempotent — no churn on repeat.
func TestRestoreSkipsWhenAlreadyAtOriginal(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "healthy")
	machine := mkDelMachine("node-departing")
	machine.Annotations[ReplicaScaleOrigAnnotation] = "pvc-x=1"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	r.restoreReplicaWants(context.Background(), c, machine)
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 1 {
		t.Fatalf("expected want to stay at 1, got %d", vol.Spec.NumberOfReplicas)
	}
}

// TestRestoreNoopWithoutAnnotation: machines without the annotation cost
// nothing.
func TestRestoreNoopWithoutAnnotation(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 5, "healthy")
	machine := mkDelMachine("node-departing")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, machine).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	r.restoreReplicaWants(context.Background(), c, machine)
	vol = getVolAfter(t, c, "pvc-x")
	if vol.Spec.NumberOfReplicas != 5 {
		t.Fatalf("expected want untouched, got %d", vol.Spec.NumberOfReplicas)
	}
}

// TestHealUncordonsFailedSourceNode: the source replica failed (instance
// manager evicted/killed) while the volume still has no surviving copy and the
// node still exists cordoned — the controller uncordons it so Longhorn can
// recreate the instance manager and salvage the replica from its intact disk.
func TestHealUncordonsFailedSourceNode(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	src := mkReplica("pvc-x", "node-departing", "running", "2026-10-01T20:10:18Z")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-departing"}}
	node.Spec.Unschedulable = true

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src, node).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	healed, err := r.healFailedSourceReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !healed {
		t.Fatal("expected the node to be uncordoned")
	}
	var nodeAfter corev1.Node
	if err := c.Get(context.Background(), types.NamespacedName{Name: "node-departing"}, &nodeAfter); err != nil {
		t.Fatal(err)
	}
	if nodeAfter.Spec.Unschedulable {
		t.Fatal("expected node uncordoned after heal")
	}
}

// TestHealSkipsWhenSurvivorHoldsData: no urgency when a surviving replica
// already holds the data — the node stays cordoned.
func TestHealSkipsWhenSurvivorHoldsData(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 2, "degraded")
	src := mkReplica("pvc-x", "node-departing", "running", "2026-10-01T20:10:18Z")
	survivor := mkReplica("pvc-x", "node-alive", "running", "")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-departing"}}
	node.Spec.Unschedulable = true

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src, survivor, node).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	healed, err := r.healFailedSourceReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if healed {
		t.Fatal("expected no heal when a survivor holds the data")
	}
	var nodeAfter corev1.Node
	if err := c.Get(context.Background(), types.NamespacedName{Name: "node-departing"}, &nodeAfter); err != nil {
		t.Fatal(err)
	}
	if !nodeAfter.Spec.Unschedulable {
		t.Fatal("expected node to stay cordoned")
	}
}

// TestHealSkipsWhenNodeGone: a failed source replica whose node no longer
// exists in the workload cluster is beyond salvage — nothing to do.
func TestHealSkipsWhenNodeGone(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	src := mkReplica("pvc-x", "node-gone", "running", "2026-10-01T20:10:18Z")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	healed, err := r.healFailedSourceReplicas(context.Background(), c, "node-gone", nil)
	if err != nil {
		t.Fatal(err)
	}
	if healed {
		t.Fatal("expected no heal when the node is gone")
	}
}

// TestHealSkipsWhenUncordoned: the node is up and schedulable — Longhorn's own
// salvage machinery is free to act; the controller does nothing.
func TestHealSkipsWhenUncordoned(t *testing.T) {
	s := evRelocateScheme(t)
	vol := mkVolume("pvc-x", 1, "degraded")
	src := mkReplica("pvc-x", "node-departing", "running", "2026-10-01T20:10:18Z")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-departing"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vol, src, node).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system", APIReader: c}

	healed, err := r.healFailedSourceReplicas(context.Background(), c, "node-departing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if healed {
		t.Fatal("expected no heal for an already-schedulable node")
	}
}
