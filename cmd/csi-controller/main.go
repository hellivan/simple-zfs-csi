// Command csi-controller is the zfs-shares CSI controller plugin. It runs as an
// unprivileged cluster-wide Deployment alongside the external-provisioner
// sidecar and implements the CSI Identity + Controller services by writing the
// ZFS-centric CRDs (ZfsVolume + ZfsShare). It hosts no reconcile loops: the node
// agent creates the datasets and the operator renders the exports.
package main

import (
	"flag"
	"os"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/container-storage-interface/spec/lib/go/csi"
	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	zfscsi "github.com/hellivan/zfs-shares/internal/csi"
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
		endpoint         string
		driverName       string
		defaultsFile     string
		annotationPrefix string
		createTimeout    time.Duration
		pollInterval     time.Duration
	)
	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint the plugin listens on.")
	flag.StringVar(&driverName, "driver-name", "zfs-shares.io", "CSI driver name; must match the CSIDriver object and StorageClass provisioner.")
	flag.StringVar(&defaultsFile, "default-parameters-file", "", "Optional YAML file with provisioner default parameters (lowest precedence).")
	flag.StringVar(&annotationPrefix, "pvc-annotation-prefix", "param.zfs-shares.io/", "PVC annotation prefix whose keys override parameters (empty disables the PVC layer).")
	flag.DurationVar(&createTimeout, "create-timeout", 2*time.Minute, "How long CreateVolume waits for a ZfsVolume to become Ready.")
	flag.DurationVar(&pollInterval, "poll-interval", 2*time.Second, "How often CreateVolume re-reads a ZfsVolume while waiting for Ready.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)
	setupLog := ctrl.Log.WithName("setup")

	defaultParams, err := loadDefaults(defaultsFile)
	if err != nil {
		setupLog.Error(err, "unable to load default parameters", "file", defaultsFile)
		os.Exit(1)
	}

	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build kubernetes client")
		os.Exit(1)
	}

	ids := &zfscsi.IdentityServer{
		DriverName: driverName,
		Version:    version,
		Capabilities: []*csi.PluginCapability{
			zfscsi.ControllerServiceCapability(),
			zfscsi.VolumeExpansionCapability(),
		},
	}
	cs := &zfscsi.ControllerServer{
		Client:           cl,
		DefaultParams:    defaultParams,
		AnnotationPrefix: annotationPrefix,
		CreateTimeout:    createTimeout,
		PollInterval:     pollInterval,
		Log:              ctrl.Log.WithName("controller"),
	}

	setupLog.Info("starting CSI controller", "driver", driverName, "endpoint", endpoint, "version", version)
	if err := zfscsi.Serve(ctrl.SetupSignalHandler(), endpoint, ids, cs, nil, ctrl.Log.WithName("grpc")); err != nil {
		setupLog.Error(err, "CSI server exited with error")
		os.Exit(1)
	}
}

// loadDefaults reads an optional YAML map of default parameters. A missing path
// yields an empty map.
func loadDefaults(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
