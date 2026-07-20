// Command zpool-discovery is the per-node storage agent. It runs as a
// privileged DaemonSet on every storage node and hosts two responsibilities:
// Tier 1 pool discovery (enumerating locally imported ZFS pools and publishing
// their identity, routing and health into cluster-scoped ZfsPool objects) and
// the ZfsDataset allocation reconciler (creating/destroying datasets and zvols
// for volumes whose pool is currently hosted on this node).
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/controller"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
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
		nodeName     string
		nodeIP       string
		zpoolBin     string
		zfsBin       string
		hostExecMode string
		hostRoot     string
		nsenterPID   int
		interval     time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Node this discovery runs on (defaults to $NODE_NAME).")
	flag.StringVar(&nodeIP, "node-ip", os.Getenv("NODE_IP"), "Routable node IP published to status.currentIP (defaults to $NODE_IP).")
	flag.StringVar(&zpoolBin, "zpool-bin", "zpool", "zpool binary name/path (resolved on the host when host-exec is on).")
	flag.StringVar(&zfsBin, "zfs-bin", "zfs", "zfs binary name/path (resolved on the host when host-exec is on).")
	flag.StringVar(&hostExecMode, "host-exec-mode", "", "How to run the host's own version-matched zpool/zfs: \"chroot\" (enter the host root at --host-root), \"nsenter\" (enter --nsenter-target-pid namespaces), or empty to run the in-image tools directly.")
	flag.StringVar(&hostRoot, "host-root", "/host", "Container-visible mount of the host root filesystem (chroot mode).")
	flag.IntVar(&nsenterPID, "nsenter-target-pid", 1, "Host PID whose namespaces are entered (nsenter mode).")
	flag.DurationVar(&interval, "poll-interval", 30*time.Second, "How often to re-discover local pools.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)
	setupLog := ctrl.Log.WithName("setup")

	if nodeName == "" {
		setupLog.Error(nil, "node name is required; set --node-name or the NODE_NAME env var")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		// One reporter per node; no cross-node coordination required.
		LeaderElection: false,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	hostExec := zpool.HostExec{
		Mode:      hostExecMode,
		HostRoot:  hostRoot,
		TargetPID: nsenterPID,
	}
	// Wrap the base runner so LoggingRunner sees the fully resolved host command
	// (chroot/nsenter prefix + version-matched binary). Enable with
	// --zap-log-level=debug to see every zfs/zpool invocation the agent runs.
	hostRunner := hostExec.BuildRunner(zpool.LoggingRunner(nil, ctrl.Log.WithName("hostcmd")))
	reporter := &controller.PoolReporter{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
		NodeIP:   nodeIP,
		Discoverer: &zpool.Discoverer{
			ZpoolBin: zpoolBin,
			ZfsBin:   zfsBin,
			Run:      hostRunner,
		},
		Interval: interval,
	}
	if err := mgr.Add(reporter); err != nil {
		setupLog.Error(err, "unable to add pool reporter")
		os.Exit(1)
	}

	if err := (&controller.ZfsDatasetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NodeName: nodeName,
		ZFS:      &zpool.CLI{Bin: zfsBin, Run: hostRunner},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up ZfsDataset reconciler")
		os.Exit(1)
	}

	if err := (&controller.ZfsSnapshotReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		NodeName: nodeName,
		ZFS:      &zpool.CLI{Bin: zfsBin, Run: hostRunner},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up ZfsSnapshot reconciler")
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

	setupLog.Info("starting storage agent", "node", nodeName, "ip", nodeIP, "interval", interval, "hostExecMode", hostExecMode)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
