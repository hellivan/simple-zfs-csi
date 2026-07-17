// Command operator is the cluster-wide control-plane manager. It runs as a
// single-active (leader-elected) Deployment anywhere in the cluster and hosts
// the unprivileged, cluster-scoped reconcilers:
//   - the pool watcher: watches core Node objects and forcibly marks the
//     ZfsPool objects of a dead storage node as NODE_OFFLINE so CSI node
//     plugins fail fast instead of hanging on an unreachable target.
//   - the ZfsShare translator: resolves a ZFS-centric ZfsShare (pool GUID +
//     dataset) to the pool's current node and renders a node-pinned
//     NetworkExport, re-targeting it when the pool moves.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr  string
		probeAddr    string
		leaderElect  bool
		leaderElecNS string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election so only one operator acts at a time.")
	flag.StringVar(&leaderElecNS, "leader-election-namespace", os.Getenv("POD_NAMESPACE"), "Namespace for the leader-election lease (defaults to $POD_NAMESPACE).")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "operator.storage.simple-zfs-csi.io",
		LeaderElectionNamespace: leaderElecNS,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.PoolWatcher{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up pool watcher")
		os.Exit(1)
	}

	if err := (&controller.ZfsShareReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up zfsshare reconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting operator", "leaderElection", leaderElect)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
