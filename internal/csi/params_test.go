package csi

import (
	"testing"

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
)

func TestResolveParameters_Inheritance(t *testing.T) {
	defaults := map[string]string{
		"poolGUID": "default-pool",
		"protocol": "nfs",
	}
	scParams := map[string]string{
		"protocol":                         "nvmeof",
		"volblocksize":                     "16k",
		"csi.storage.k8s.io/pvc/name":      "my-pvc",
		"csi.storage.k8s.io/fstype":        "ext4",
		"csi.storage.k8s.io/pv/name":       "pvc-abc",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
	}
	pvcAnnotations := map[string]string{
		"param.zfs-shares.io/poolGUID": "pvc-pool",
		"unrelated/annotation":         "ignored",
	}

	merged := ResolveParameters(defaults, scParams, pvcAnnotations, "param.zfs-shares.io/")

	// PVC annotation wins over SC over defaults.
	if merged["poolGUID"] != "pvc-pool" {
		t.Errorf("poolGUID = %q, want pvc-pool", merged["poolGUID"])
	}
	// SC wins over default.
	if merged["protocol"] != "nvmeof" {
		t.Errorf("protocol = %q, want nvmeof", merged["protocol"])
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
		map[string]string{"poolGUID": "d"},
		map[string]string{"protocol": "nfs"},
		map[string]string{"param.zfs-shares.io/poolGUID": "should-be-ignored"},
		"", // disabled
	)
	if merged["poolGUID"] != "d" {
		t.Errorf("poolGUID = %q, want d (annotation layer disabled)", merged["poolGUID"])
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
