package controller

import (
	"context"
	"testing"

	longhornv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"time"
)

func mkPod(name, ns, node string, ownerKind string, withPVC bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Volumes:  []corev1.Volume{},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ownerKind != "" {
		t := true
		pod.OwnerReferences = []metav1.OwnerReference{{Kind: ownerKind, APIVersion: "apps/v1", Name: name + "-" + ownerKind, UID: "x", Controller: &t}}
	}
	if withPVC {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name + "-pvc"},
			},
		})
	}
	return pod
}

func mkLHVolumeHealthy(name string) *longhornv1beta2.Volume {
	return &longhornv1beta2.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "longhorn-system"},
		Status:     longhornv1beta2.VolumeStatus{Robustness: longhornv1beta2.VolumeRobustnessHealthy},
	}
}

func mkLHVolumeFaulted(name string) *longhornv1beta2.Volume {
	return &longhornv1beta2.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "longhorn-system"},
		Status:     longhornv1beta2.VolumeStatus{Robustness: longhornv1beta2.VolumeRobustnessFaulted},
	}
}

func mkPVC(name, ns, volName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volName},
	}
}

// mkMachine builds a deleting Machine with a NodeRef for tests.
func mkMachine(nodeName string) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "fleet-default",
			Annotations: map[string]string{
				HookReleasedAnnotation: metav1.Now().Format(time.RFC3339),
			},
		},
		Spec: clusterv1.MachineSpec{ClusterName: "test-cluster"},
	}
	now := metav1.Now()
	m.DeletionTimestamp = &now
	m.Status.NodeRef = &corev1.ObjectReference{Kind: "Node", Name: nodeName}
	return m
}

// A Running pod holding a healthy Longhorn PVC on the departing node gets evicted.
func TestEvictStuckPodHealthy(t *testing.T) {
	s := evScheme(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "departing"}}
	pod := mkPod("writer", "default", "departing", "", true)
	pvc := mkPVC("writer-pvc", "default", "pvc-vol-1")
	vol := mkLHVolumeHealthy("pvc-vol-1")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, pod, pvc, vol).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	r.releaseStuckAttachments(context.Background(), c, mkMachine("departing"))

	var got corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Name: "writer", Namespace: "default"}, &got)
	if err == nil {
		t.Fatal("expected the stuck pod to be deleted")
	}

	// Node should be cordoned.
	var n corev1.Node
	_ = c.Get(context.Background(), types.NamespacedName{Name: "departing"}, &n)
	if !n.Spec.Unschedulable {
		t.Fatal("expected departing node to be cordoned")
	}
}

// A pod holding a FAULTED Longhorn volume must NOT be evicted.
func TestEvictStuckPodSkipsFaulted(t *testing.T) {
	s := evScheme(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "departing"}}
	pod := mkPod("writer", "default", "departing", "", true)
	pvc := mkPVC("writer-pvc", "default", "pvc-vol-2")
	vol := mkLHVolumeFaulted("pvc-vol-2")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, pod, pvc, vol).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	r.releaseStuckAttachments(context.Background(), c, mkMachine("departing"))

	var got corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "writer", Namespace: "default"}, &got); err != nil {
		t.Fatal("pod holding a FAULTED volume must not be deleted")
	}
}

// DaemonSet pods on the departing node must NOT be touched.
func TestEvictStuckPodSkipsDaemonSets(t *testing.T) {
	s := evScheme(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "departing"}}
	dsPod := mkPod("csi-plugin", "longhorn-system", "departing", "DaemonSet", true)
	pvc := mkPVC("csi-plugin-pvc", "longhorn-system", "pvc-vol-3")
	vol := mkLHVolumeHealthy("pvc-vol-3")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, dsPod, pvc, vol).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	r.releaseStuckAttachments(context.Background(), c, mkMachine("departing"))

	var got corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "csi-plugin", Namespace: "longhorn-system"}, &got); err != nil {
		t.Fatal("DaemonSet pod must not be deleted")
	}
}

// A pod whose PVC is not backed by Longhorn (no Volume CR resolves) must NOT
// be deleted: EvictStuckPods clears Longhorn attachments only — force-deleting
// unrelated workloads is out of scope (per the flag and README documentation).
func TestEvictStuckPodSkipsNonLonghornPVC(t *testing.T) {
	s := evScheme(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "departing"}}
	pod := mkPod("local-pv-app", "default", "departing", "", true)
	pvc := mkPVC("local-pv-app-pvc", "default", "some-local-pv")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, pod, pvc).Build()
	r := &LonghornEvictionReconciler{Client: c, LonghornNS: "longhorn-system"}

	r.releaseStuckAttachments(context.Background(), c, mkMachine("departing"))

	var got corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "local-pv-app", Namespace: "default"}, &got); err != nil {
		t.Fatal("non-Longhorn PVC pod must not be deleted")
	}
}
