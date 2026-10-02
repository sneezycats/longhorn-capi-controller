package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	"github.com/sneezycats/longhorn-capi-controller/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	// NOTE: Longhorn types are NOT added to the management scheme.
	// They are registered in a separate per-workload-cluster scheme.
}

func main() {
	var (
		evictionTimeout      time.Duration
		pollInterval         time.Duration
		longhornNamespace    string
		gcInterval           time.Duration
		earlyRelease         bool
		evictStuckPods       bool
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
	)

	// Resolve env overrides with sane precedence: explicit CLI flag wins,
	// otherwise check env, otherwise default.
	flag.DurationVar(&evictionTimeout, "eviction-timeout", envDuration("EVICTION_TIMEOUT", 2*time.Hour),
		"Maximum time to wait for Longhorn eviction before releasing the hook. Must exceed the largest volume rebuild time.")
	flag.DurationVar(&pollInterval, "poll-interval", envDuration("POLL_INTERVAL", 15*time.Second),
		"How frequently to re-check Longhorn node status during eviction.")
	var kubeconfigSuffix string
	flag.StringVar(&kubeconfigSuffix, "workload-kubeconfig-suffix", envString("WORKLOAD_KUBECONFIG_SUFFIX", "lhcc"),
		"Suffix for the dedicated per-cluster least-privilege kubeconfig Secret (<cluster>-<suffix>-kubeconfig); the shared <cluster>-kubeconfig Secret is the fallback. Empty disables the dedicated lookup.")
	flag.StringVar(&longhornNamespace, "longhorn-namespace", envString("LONGHORN_NAMESPACE", "longhorn-system"),
		"Namespace where Longhorn is deployed in workload clusters.")
	flag.DurationVar(&gcInterval, "gc-interval", envDuration("GC_INTERVAL", 5*time.Minute),
		"How frequently to sweep for orphaned nodes.longhorn.io CRs whose k8s Node is gone.")
	flag.BoolVar(&earlyRelease, "early-release", envBool("EARLY_RELEASE", true),
		"Release the pre-terminate hook early when eviction has drained but the rebuild is blocked only by the departing node's membership (all volumes degraded-but-safe). Skips the eviction-timeout burn.")
	flag.BoolVar(&evictStuckPods, "evict-stuck-pods", envBool("EVICT_STUCK_PODS", true),
		"After hook release, cordon the departing node and force-delete non-DaemonSet pods holding non-faulted Longhorn PVCs so the volume detaches and CAPI's WaitingForVolumeDetach completes.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", envString("METRICS_BIND_ADDRESS", ":8080"),
		"The address the metrics endpoint binds to. Use :8443 for HTTPS or 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", envString("HEALTH_PROBE_BIND_ADDRESS", ":8081"),
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", envBool("LEADER_ELECT", true),
		"Enable leader election for controller manager.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "longhorn-capi-eviction-controller",
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.LonghornEvictionReconciler{
		Client:                 mgr.GetClient(),
		APIReader:              mgr.GetAPIReader(),
		Scheme:                 mgr.GetScheme(),
		Recorder:               mgr.GetEventRecorderFor("longhorn-capi-eviction-controller"),
		EvictionTimeout:        evictionTimeout,
		PollInterval:           pollInterval,
		LonghornNS:             longhornNamespace,
		KubeconfigSecretSuffix: kubeconfigSuffix,
		EarlyRelease:           earlyRelease,
		EvictStuckPods:         evictStuckPods,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Machine")
		os.Exit(1)
	}

	if err := (&controller.LonghornNodeGCReconciler{
		Client:                 mgr.GetClient(),
		APIReader:              mgr.GetAPIReader(),
		Scheme:                 mgr.GetScheme(),
		Recorder:               mgr.GetEventRecorderFor("longhorn-capi-eviction-controller"),
		GCInterval:             gcInterval,
		LonghornNS:             longhornNamespace,
		KubeconfigSecretSuffix: kubeconfigSuffix,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "LonghornNodeGC")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager",
		"evictionTimeout", evictionTimeout,
		"pollInterval", pollInterval,
		"longhornNamespace", longhornNamespace,
		"metricsAddr", metricsAddr,
		"probeAddr", probeAddr,
		"leaderElection", enableLeaderElection,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

// env helpers — CLI flags default to these resolved values so that env acts as the
// default without requiring an explicit flag. If the user passes the flag,
// flag.Parse overwrites it.

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: invalid duration for %s=%q: %v — using default %s\n", key, v, err, fallback)
			return fallback
		}
		return d
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "False", "no", "NO", "off", "OFF":
		return false
	default:
		fmt.Fprintf(os.Stderr, "WARNING: invalid bool for %s=%q — using default %v\n", key, v, fallback)
		return fallback
	}
}
