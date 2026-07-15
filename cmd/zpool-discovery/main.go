// Command zpool-discovery is the Tier 1 monitor. It runs as a privileged
// DaemonSet on every storage node, periodically enumerating the ZFS pools
// imported locally and publishing their identity, routing (node/IP/mountpoint)
// and health into cluster-scoped ZfsPool objects.
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

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	"github.com/hellivan/zfs-shares/internal/controller"
	"github.com/hellivan/zfs-shares/internal/zpool"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr string
		probeAddr   string
		nodeName    string
		nodeIP      string
		zpoolBin    string
		zfsBin      string
		interval    time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Node this discovery runs on (defaults to $NODE_NAME).")
	flag.StringVar(&nodeIP, "node-ip", os.Getenv("NODE_IP"), "Routable node IP published to status.currentIP (defaults to $NODE_IP).")
	flag.StringVar(&zpoolBin, "zpool-bin", "zpool", "Path to the zpool binary.")
	flag.StringVar(&zfsBin, "zfs-bin", "zfs", "Path to the zfs binary.")
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

	reporter := &controller.PoolReporter{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
		NodeIP:   nodeIP,
		Discoverer: &zpool.Discoverer{
			ZpoolBin: zpoolBin,
			ZfsBin:   zfsBin,
			Run:      nil, // real exec runner
		},
		Interval: interval,
	}
	if err := mgr.Add(reporter); err != nil {
		setupLog.Error(err, "unable to add pool reporter")
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

	setupLog.Info("starting zpool-discovery", "node", nodeName, "ip", nodeIP, "interval", interval)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
