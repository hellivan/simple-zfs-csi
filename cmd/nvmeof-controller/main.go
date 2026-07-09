// Command nvmeof-controller reconciles nvmeof-protocol ZfsShare objects for the
// node it runs on into the kernel NVMe target (nvmet) via configfs.
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

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	"github.com/hellivan/zfs-shares/internal/controller"
	"github.com/hellivan/zfs-shares/internal/nvmet"
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
		configfsRoot string
		nqnPrefix    string
		portID       string
		trAddr       string
		trSvcID      string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Node this controller manages (defaults to $NODE_NAME).")
	flag.StringVar(&configfsRoot, "nvmet-configfs-root", nvmet.DefaultRoot, "Root of the nvmet configfs tree.")
	flag.StringVar(&nqnPrefix, "nqn-prefix", "nqn.2025-01.io.zfs-shares", "Prefix for derived subsystem NQNs.")
	flag.StringVar(&portID, "port-id", "1", "nvmet port directory id.")
	flag.StringVar(&trAddr, "transport-address", "0.0.0.0", "NVMe-oF TCP listen address.")
	flag.StringVar(&trSvcID, "transport-service-id", "4420", "NVMe-oF TCP listen port.")

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

	target := nvmet.NewTarget(configfsRoot, nvmet.PortConfig{
		ID:      portID,
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  trAddr,
		TrSvcID: trSvcID,
	})
	if err := target.Available(); err != nil {
		setupLog.Error(err, "nvmet target unavailable")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.NVMeoFReconciler{
		Client:    mgr.GetClient(),
		NodeName:  nodeName,
		Target:    target,
		NQNPrefix: nqnPrefix,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up NVMe-oF reconciler")
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

	setupLog.Info("starting nvmeof-controller", "node", nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
