package csi

import (
	"testing"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func TestResolveParameters_Inheritance(t *testing.T) {
	defaults := map[string]string{
		"poolGUID":      "default-pool", // StorageClass-only: must be dropped
		"datasetPrefix": "default/pfx",  // StorageClass-only: must be dropped
		"protocol":      "nfs",
		"nfsOptions":    "ro",
	}
	scParams := map[string]string{
		"poolGUID":                         "sc-pool",
		"datasetPrefix":                    "k8s",
		"protocol":                         "nvmeof",
		"volblocksize":                     "16k",
		"csi.storage.k8s.io/pvc/name":      "my-pvc",
		"csi.storage.k8s.io/fstype":        "ext4",
		"csi.storage.k8s.io/pv/name":       "pvc-abc",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
	}
	pvcAnnotations := map[string]string{
		"param.simple-zfs-csi.io/poolGUID":      "pvc-pool", // StorageClass-only: must be dropped
		"param.simple-zfs-csi.io/datasetPrefix": "pvc/pfx",  // StorageClass-only: must be dropped
		"param.simple-zfs-csi.io/nfsOptions":    "rw",
		"unrelated/annotation":                  "ignored",
	}

	merged := ResolveParameters(defaults, scParams, pvcAnnotations, "param.simple-zfs-csi.io/")

	// poolGUID is StorageClass-only: neither defaults nor PVC annotations win.
	if merged["poolGUID"] != "sc-pool" {
		t.Errorf("poolGUID = %q, want sc-pool (StorageClass-only)", merged["poolGUID"])
	}
	// datasetPrefix is StorageClass-only too.
	if merged["datasetPrefix"] != "k8s" {
		t.Errorf("datasetPrefix = %q, want k8s (StorageClass-only)", merged["datasetPrefix"])
	}
	// SC wins over default.
	if merged["protocol"] != "nvmeof" {
		t.Errorf("protocol = %q, want nvmeof", merged["protocol"])
	}
	// PVC annotation overrides defaults for non-restricted keys.
	if merged["nfsOptions"] != "rw" {
		t.Errorf("nfsOptions = %q, want rw (PVC annotation wins)", merged["nfsOptions"])
	}
	// SC-only value passes through.
	if merged["volblocksize"] != "16k" {
		t.Errorf("volblocksize = %q, want 16k", merged["volblocksize"])
	}
	// Reserved csi.storage.k8s.io/* keys are stripped.
	for k := range merged {
		if _, bad := scParams[k]; bad && len(k) > len("csi.storage.k8s.io/") && k[:len("csi.storage.k8s.io/")] == "csi.storage.k8s.io/" {
			t.Errorf("reserved key %q leaked into merged params", k)
		}
	}
	if _, ok := merged["fstype"]; ok {
		t.Errorf("stripped reserved key should not appear as fstype")
	}
	// Non-prefixed PVC annotations are ignored.
	if _, ok := merged["annotation"]; ok {
		t.Errorf("non-prefixed annotation leaked into params")
	}
}

func TestResolveParameters_NoAnnotationLayer(t *testing.T) {
	merged := ResolveParameters(
		map[string]string{"nfsOptions": "ro"},
		map[string]string{"poolGUID": "sc-pool", "protocol": "nfs"},
		map[string]string{"param.simple-zfs-csi.io/nfsOptions": "should-be-ignored"},
		"", // disabled
	)
	if merged["nfsOptions"] != "ro" {
		t.Errorf("nfsOptions = %q, want ro (annotation layer disabled)", merged["nfsOptions"])
	}
	if merged["poolGUID"] != "sc-pool" {
		t.Errorf("poolGUID = %q, want sc-pool", merged["poolGUID"])
	}
}

func TestResolveParameters_StorageClassOnly(t *testing.T) {
	// poolGUID/datasetPrefix supplied only via defaults and PVC annotations must
	// be dropped entirely, leaving the required poolGUID unset.
	merged := ResolveParameters(
		map[string]string{"poolGUID": "from-default", "datasetPrefix": "from-default"},
		map[string]string{"protocol": "nfs"},
		map[string]string{
			"param.simple-zfs-csi.io/poolGUID":      "from-pvc",
			"param.simple-zfs-csi.io/datasetPrefix": "from-pvc",
		},
		"param.simple-zfs-csi.io/",
	)
	if _, ok := merged["poolGUID"]; ok {
		t.Errorf("poolGUID leaked from non-StorageClass layer: %q", merged["poolGUID"])
	}
	if _, ok := merged["datasetPrefix"]; ok {
		t.Errorf("datasetPrefix leaked from non-StorageClass layer: %q", merged["datasetPrefix"])
	}
	if _, err := ParseParams(merged); err == nil {
		t.Errorf("expected ParseParams error when poolGUID only came from non-SC layers")
	}
}

func TestParseParams_NFSDerivesFilesystem(t *testing.T) {
	rp, err := ParseParams(map[string]string{
		"poolGUID":             "999",
		"protocol":             "nfs",
		"datasetPrefix":        "/k8s/",
		"property.compression": "lz4",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rp.Protocol != storagev1alpha1.ProtocolNFS {
		t.Errorf("protocol = %q, want nfs", rp.Protocol)
	}
	if rp.VolumeType != storagev1alpha1.VolumeTypeFilesystem {
		t.Errorf("volumeType = %q, want filesystem", rp.VolumeType)
	}
	if rp.DatasetPrefix != "k8s" {
		t.Errorf("datasetPrefix = %q, want k8s (trimmed)", rp.DatasetPrefix)
	}
	if rp.Properties["compression"] != "lz4" {
		t.Errorf("properties[compression] = %q, want lz4", rp.Properties["compression"])
	}
	if got := rp.Dataset("pvc-1"); got != "k8s/pvc-1" {
		t.Errorf("Dataset = %q, want k8s/pvc-1", got)
	}
	// Default NFS client is "*".
	if len(rp.NFSClients) != 1 || rp.NFSClients[0].Client != "*" {
		t.Errorf("NFSClients = %+v, want single '*'", rp.NFSClients)
	}
}

func TestParseParams_NVMeoFDerivesVolume(t *testing.T) {
	rp, err := ParseParams(map[string]string{
		"poolGUID":           "999",
		"protocol":           "nvmeof",
		"volblocksize":       "16k",
		"nvmeofAllowedHosts": "nqn.host-a, nqn.host-b",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rp.VolumeType != storagev1alpha1.VolumeTypeVolume {
		t.Errorf("volumeType = %q, want volume", rp.VolumeType)
	}
	if rp.Volblocksize != "16k" {
		t.Errorf("volblocksize = %q, want 16k", rp.Volblocksize)
	}
	if len(rp.NVMeoFAllowedHosts) != 2 || rp.NVMeoFAllowedHosts[0] != "nqn.host-a" || rp.NVMeoFAllowedHosts[1] != "nqn.host-b" {
		t.Errorf("allowedHosts = %+v, want [nqn.host-a nqn.host-b]", rp.NVMeoFAllowedHosts)
	}
	// No prefix -> dataset is just the volume name.
	if got := rp.Dataset("pvc-2"); got != "pvc-2" {
		t.Errorf("Dataset = %q, want pvc-2", got)
	}
}

func TestParseParams_NFSClientsWithOptions(t *testing.T) {
	rp, err := ParseParams(map[string]string{
		"poolGUID":   "999",
		"protocol":   "nfs",
		"nfsClients": "10.0.0.0/8:rw;no_root_squash, 192.168.1.5",
		"nfsOptions": "ro sync",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rp.NFSClients) != 2 {
		t.Fatalf("NFSClients len = %d, want 2", len(rp.NFSClients))
	}
	c0 := rp.NFSClients[0]
	if c0.Client != "10.0.0.0/8" || len(c0.Options) != 2 || c0.Options[0] != "rw" || c0.Options[1] != "no_root_squash" {
		t.Errorf("client0 = %+v, want 10.0.0.0/8 rw;no_root_squash", c0)
	}
	c1 := rp.NFSClients[1]
	if c1.Client != "192.168.1.5" || len(c1.Options) != 2 || c1.Options[0] != "ro" || c1.Options[1] != "sync" {
		t.Errorf("client1 = %+v, want 192.168.1.5 with default options ro sync", c1)
	}
}

func TestParseParams_Errors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing poolGUID": {"protocol": "nfs"},
		"missing protocol": {"poolGUID": "999"},
		"bad protocol":     {"poolGUID": "999", "protocol": "smb"},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseParams(params); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}
