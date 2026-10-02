package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func kubeconfigSecret(name string, kubeconfig string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "fleet-default"},
		Data:       map[string][]byte{"value": []byte(kubeconfig)},
	}
}

func TestResolvePrefersDedicatedSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(evScheme(t)).WithObjects(
		kubeconfigSecret("c1-lhcc-kubeconfig", "dedicated"),
		kubeconfigSecret("c1-kubeconfig", "shared"),
	).Build()
	data, name, err := resolveKubeconfigSecretData(context.Background(), c, "c1", "fleet-default", "lhcc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "c1-lhcc-kubeconfig" {
		t.Fatalf("expected dedicated secret, got %q", name)
	}
	if string(data) != "dedicated" {
		t.Fatalf("expected dedicated kubeconfig bytes, got %q", string(data))
	}
}

func TestResolveFallsBackToSharedSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(evScheme(t)).WithObjects(
		kubeconfigSecret("c1-kubeconfig", "shared"),
	).Build()
	data, name, err := resolveKubeconfigSecretData(context.Background(), c, "c1", "fleet-default", "lhcc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "c1-kubeconfig" {
		t.Fatalf("expected shared secret fallback, got %q", name)
	}
	if string(data) != "shared" {
		t.Fatalf("expected shared kubeconfig bytes, got %q", string(data))
	}
}

func TestResolveEmptySuffixUsesSharedOnly(t *testing.T) {
	// A dedicated Secret present but the suffix disabled: the shared Secret
	// must be used (backwards-compatible behavior).
	c := fake.NewClientBuilder().WithScheme(evScheme(t)).WithObjects(
		kubeconfigSecret("c1-lhcc-kubeconfig", "dedicated"),
		kubeconfigSecret("c1-kubeconfig", "shared"),
	).Build()
	_, name, err := resolveKubeconfigSecretData(context.Background(), c, "c1", "fleet-default", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "c1-kubeconfig" {
		t.Fatalf("expected shared secret when suffix is empty, got %q", name)
	}
}

func TestResolveMissingSecretsIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(evScheme(t)).Build()
	_, _, err := resolveKubeconfigSecretData(context.Background(), c, "c1", "fleet-default", "lhcc")
	if err == nil {
		t.Fatal("expected error when no kubeconfig Secret exists")
	}
}

func TestKubeconfigSecretNames(t *testing.T) {
	if got := kubeconfigSecretNames("c1", "lhcc"); got[0] != "c1-lhcc-kubeconfig" || got[1] != "c1-kubeconfig" {
		t.Fatalf("unexpected names: %v", got)
	}
	if got := kubeconfigSecretNames("c1", ""); len(got) != 1 || got[0] != "c1-kubeconfig" {
		t.Fatalf("empty suffix must yield only the shared name, got %v", got)
	}
}
