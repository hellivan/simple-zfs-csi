// Command nfs-controller reconciles nfs-protocol NetworkExport objects for the node
// it runs on and serves them from an in-container NFS server.
package main

import (
	"context"
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
	"github.com/hellivan/simple-zfs-csi/internal/nfsserver"
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
		exportsPath  string
		nfsThreads   int
		manageServer bool
		v4Only       bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe binds to.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Node this controller manages (defaults to $NODE_NAME).")
	flag.StringVar(&exportsPath, "exports-path", "/etc/exports", "Path to the NFS exports file.")
	flag.IntVar(&nfsThreads, "nfs-threads", 8, "Number of nfsd kernel threads to start.")
	flag.BoolVar(&manageServer, "manage-nfs-server", true, "Bring up and supervise the in-container NFS server daemons.")
	flag.BoolVar(&v4Only, "nfs-v4-only", false, "Serve NFSv4 only: disable v2/v3 and skip rpcbind (leaner, single port 2049).")

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
		// One controller per node; no cross-node coordination required.
		LeaderElection: false,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if manageServer {
		srv := nfsserver.NewServer(nfsserver.ServerConfig{Threads: nfsThreads, V4Only: v4Only}, ctrl.Log.WithName("nfs-server"))
		if err := mgr.Add(runnableFunc(srv.Run)); err != nil {
			setupLog.Error(err, "unable to add NFS server supervisor")
			os.Exit(1)
		}
	}

	if err := (&controller.NFSReconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
		Exports:  nfsserver.NewExportManager(exportsPath),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up NFS reconciler")
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

	setupLog.Info("starting nfs-controller", "node", nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// runnableFunc adapts a func(context.Context) error into a manager.Runnable.
type runnableFunc func(context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }
