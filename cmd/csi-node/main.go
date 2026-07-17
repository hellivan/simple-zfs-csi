// Command csi-node is the simple-zfs-csi CSI node plugin. It runs as a privileged
// DaemonSet on every node alongside the node-driver-registrar sidecar and
// implements the CSI Identity + Node services. NodePublishVolume resolves the
// routing-only volume_context to the pool's current node/IP/mount root via
// ZfsPool.status, refuses when the storage node is offline, and mounts NFS or
// connects+mounts NVMe-oF. It writes no CRDs.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	zfscsi "github.com/hellivan/simple-zfs-csi/internal/csi"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// version is overridable at build time via -ldflags.
var version = "dev"

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
}

func main() {
	var (
		endpoint      string
		driverName    string
		nodeName      string
		nvmeTransport string
		nvmePort      string
		hostExecMode  string
		hostRoot      string
		nsenterPID    int
	)
	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint the plugin listens on.")
	flag.StringVar(&driverName, "driver-name", "simple-zfs-csi.io", "CSI driver name; must match the CSIDriver object and StorageClass provisioner.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Node this plugin runs on (defaults to $NODE_NAME).")
	flag.StringVar(&nvmeTransport, "nvme-transport", "tcp", "NVMe-oF transport used to connect to the storage node.")
	flag.StringVar(&nvmePort, "nvme-port", "4420", "NVMe-oF service port on the storage node (must match the nvmeof controller).")
	flag.StringVar(&hostExecMode, "host-exec-mode", "", "How to run mount/nvme binaries: \"chroot\" (enter the host root at --host-root), \"nsenter\" (enter --nsenter-target-pid namespaces), or empty to run the in-image tools directly.")
	flag.StringVar(&hostRoot, "host-root", "/host", "Container-visible mount of the host root filesystem (chroot mode).")
	flag.IntVar(&nsenterPID, "nsenter-target-pid", 1, "Host PID whose namespaces are entered (nsenter mode).")

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

	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build kubernetes client")
		os.Exit(1)
	}

	hostExec := zpool.HostExec{Mode: hostExecMode, HostRoot: hostRoot, TargetPID: nsenterPID}
	mounter := zfscsi.NewHostMounter(hostExec.BuildRunner(nil))

	ids := &zfscsi.IdentityServer{DriverName: driverName, Version: version}
	ns := &zfscsi.NodeServer{
		Client:        cl,
		Mounter:       mounter,
		NodeID:        nodeName,
		NVMeTransport: nvmeTransport,
		NVMePort:      nvmePort,
		Log:           ctrl.Log.WithName("node"),
	}

	setupLog.Info("starting CSI node plugin", "driver", driverName, "endpoint", endpoint, "node", nodeName, "version", version)
	if err := zfscsi.Serve(ctrl.SetupSignalHandler(), endpoint, ids, nil, ns, ctrl.Log.WithName("grpc")); err != nil {
		setupLog.Error(err, "CSI server exited with error")
		os.Exit(1)
	}
}
