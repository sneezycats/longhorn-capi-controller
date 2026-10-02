package controller

// Workload-cluster credential resolution.
//
// The controller reaches each workload cluster through a per-cluster kubeconfig
// Secret read with an UNCACHED reader (see buildWorkloadClusterClient). Two
// Secret names are considered, in order:
//
//  1. <cluster>-<suffix>-kubeconfig — the dedicated least-privilege Secret.
//     It carries a ServiceAccount credential bound to the least-privilege
//     workload ClusterRole (see config/rbac/workload_role.yaml) instead of
//     the admin credential the shared Secret holds.
//  2. <cluster>-kubeconfig — the CAPI/Rancher-convention shared Secret. This
//     is Rancher's own managed credential: the drainer and the kubeconfig
//     manager both consume it, and Rancher regenerates it. It is the
//     fallback for clusters that have not been migrated to the dedicated
//     Secret yet.
//
// An empty suffix disables the dedicated lookup entirely.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kubeconfigSecretNames returns the ordered candidate Secret names for a
// workload cluster's credential: dedicated first, shared fallback. An empty
// suffix yields only the shared name.
func kubeconfigSecretNames(clusterName, suffix string) []string {
	if suffix == "" {
		return []string{clusterName + "-kubeconfig"}
	}
	return []string{clusterName + "-" + suffix + "-kubeconfig", clusterName + "-kubeconfig"}
}

// resolveKubeconfigSecretData reads the cluster's workload credential and
// returns the kubeconfig bytes plus the name of the Secret that supplied
// them. Missing dedicated Secrets are a normal fallback, not an error; other
// read failures abort immediately.
func resolveKubeconfigSecretData(ctx context.Context, secretReader client.Reader, clusterName, namespace, suffix string) ([]byte, string, error) {
	var lastErr error
	for _, secretName := range kubeconfigSecretNames(clusterName, suffix) {
		secret := &corev1.Secret{}
		err := secretReader.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret)
		if apierrors.IsNotFound(err) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("fetching kubeconfig secret %s/%s: %w", namespace, secretName, err)
		}
		data, ok := secret.Data["value"]
		if !ok {
			return nil, "", fmt.Errorf("kubeconfig secret %s/%s missing 'value' key", namespace, secretName)
		}
		return data, secretName, nil
	}
	return nil, "", fmt.Errorf("no workload kubeconfig Secret found for cluster %s/%s (tried %v): %w",
		namespace, clusterName, kubeconfigSecretNames(clusterName, suffix), lastErr)
}
