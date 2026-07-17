// Package csi implements the simple-zfs-csi CSI controller plugin: a thin,
// unprivileged gRPC adapter that translates CSI CreateVolume/DeleteVolume calls
// into the ZFS-centric CRDs (ZfsDataset + ZfsShare) and returns a routing-only
// volume_context. It contains no reconcile loops; the agent and operator do the
// actual work.
package csi

import (
	"fmt"
	"path"
	"strings"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// Parameter keys understood by the driver. They resolve through a three-layer
// inheritance chain (provisioner defaults < StorageClass parameters < PVC
// annotations); see docs/design-decisions.md ADR-0001. poolGUID and
// datasetPrefix are StorageClass-only (ADR-0002): they are never inherited from
// the defaults or PVC-annotation layers.
const (
	ParamPoolGUID           = "poolGUID"
	ParamProtocol           = "protocol"
	ParamDatasetPrefix      = "datasetPrefix"
	ParamVolblocksize       = "volblocksize"
	ParamNFSClients         = "nfsClients"
	ParamNFSOptions         = "nfsOptions"
	ParamNVMeoFAllowedHosts = "nvmeofAllowedHosts"

	// PropertyPrefix marks a pass-through ZFS property, e.g.
	// "property.compression" -> spec.properties["compression"].
	PropertyPrefix = "property."

	// reservedParamPrefix is injected by external-provisioner (with
	// --extra-create-metadata) and must never be treated as a driver parameter.
	reservedParamPrefix = "csi.storage.k8s.io/"

	// Reserved keys carrying the source PVC identity, used to fetch PVC
	// annotations for the third inheritance layer.
	ReservedPVCName      = "csi.storage.k8s.io/pvc/name"
	ReservedPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
)

// storageClassOnlyParams may ONLY be set via StorageClass parameters. They are
// ignored if present in the provisioner-defaults layer or the PVC-annotation
// layer, so neither a cluster-wide default nor a namespace tenant can redirect
// provisioning to a different pool or dataset prefix. poolGUID additionally has
// no default and is required, so every StorageClass must name its pool
// explicitly. See docs/design-decisions.md ADR-0002.
var storageClassOnlyParams = map[string]struct{}{
	ParamPoolGUID:      {},
	ParamDatasetPrefix: {},
}

// ResolvedParams is the parsed, validated result of the parameter inheritance
// chain: everything needed to render a ZfsDataset + ZfsShare.
type ResolvedParams struct {
	PoolGUID           string
	Protocol           storagev1alpha1.Protocol
	DatasetType        storagev1alpha1.DatasetType
	DatasetPrefix      string
	Volblocksize       string
	NFSClients         []storagev1alpha1.NFSClient
	NVMeoFAllowedHosts []string
	Properties         map[string]string
}

// ResolveParameters merges the three inheritance layers into a single flat map,
// later layers overriding earlier ones:
//  1. defaults    — provisioner defaults (from --default-parameters-file);
//  2. scParams    — StorageClass parameters (reserved csi.storage.k8s.io/* keys stripped);
//  3. pvcAnnotations — PVC annotations carrying annPrefix (prefix stripped).
//
// storageClassOnlyParams (poolGUID, datasetPrefix) are honoured only from
// scParams; if they appear in the defaults or PVC-annotation layers they are
// dropped, so pool routing and the dataset prefix are fixed by the StorageClass.
func ResolveParameters(defaults, scParams, pvcAnnotations map[string]string, annPrefix string) map[string]string {
	merged := make(map[string]string, len(defaults)+len(scParams))
	for k, v := range defaults {
		if _, scOnly := storageClassOnlyParams[k]; scOnly {
			continue
		}
		merged[k] = v
	}
	for k, v := range scParams {
		if strings.HasPrefix(k, reservedParamPrefix) {
			continue
		}
		merged[k] = v
	}
	if annPrefix != "" {
		for k, v := range pvcAnnotations {
			if !strings.HasPrefix(k, annPrefix) {
				continue
			}
			key := strings.TrimPrefix(k, annPrefix)
			if _, scOnly := storageClassOnlyParams[key]; scOnly {
				continue
			}
			merged[key] = v
		}
	}
	return merged
}

// ParseParams validates and types a merged parameter map. The protocol fixes the
// ZFS dataset type (nfs -> filesystem, nvmeof -> volume/zvol); volumeMode is an
// orthogonal concern resolved by the node plugin.
func ParseParams(p map[string]string) (*ResolvedParams, error) {
	rp := &ResolvedParams{}

	rp.PoolGUID = strings.TrimSpace(p[ParamPoolGUID])
	if rp.PoolGUID == "" {
		return nil, fmt.Errorf("parameter %q is required", ParamPoolGUID)
	}

	switch storagev1alpha1.Protocol(strings.TrimSpace(p[ParamProtocol])) {
	case storagev1alpha1.ProtocolNFS:
		rp.Protocol = storagev1alpha1.ProtocolNFS
		rp.DatasetType = storagev1alpha1.DatasetTypeFilesystem
	case storagev1alpha1.ProtocolNVMeoF:
		rp.Protocol = storagev1alpha1.ProtocolNVMeoF
		rp.DatasetType = storagev1alpha1.DatasetTypeVolume
	case "":
		return nil, fmt.Errorf("parameter %q is required", ParamProtocol)
	default:
		return nil, fmt.Errorf("parameter %q must be %q or %q, got %q",
			ParamProtocol, storagev1alpha1.ProtocolNFS, storagev1alpha1.ProtocolNVMeoF, p[ParamProtocol])
	}

	rp.DatasetPrefix = strings.Trim(strings.TrimSpace(p[ParamDatasetPrefix]), "/")
	rp.Volblocksize = strings.TrimSpace(p[ParamVolblocksize])

	switch rp.Protocol {
	case storagev1alpha1.ProtocolNFS:
		rp.NFSClients = parseNFSClients(p[ParamNFSClients], p[ParamNFSOptions])
	case storagev1alpha1.ProtocolNVMeoF:
		rp.NVMeoFAllowedHosts = splitList(p[ParamNVMeoFAllowedHosts], ",")
	}

	for k, v := range p {
		if name := strings.TrimPrefix(k, PropertyPrefix); name != k && name != "" {
			if rp.Properties == nil {
				rp.Properties = map[string]string{}
			}
			rp.Properties[name] = v
		}
	}

	return rp, nil
}

// Dataset returns the logical dataset path for a CSI volume name, honouring the
// optional prefix: "<datasetPrefix>/<volName>".
func (rp *ResolvedParams) Dataset(volName string) string {
	name := strings.Trim(volName, "/")
	if rp.DatasetPrefix == "" {
		return name
	}
	return path.Join(rp.DatasetPrefix, name)
}

// parseNFSClients parses the nfsClients parameter into export client rules.
// Each comma-separated entry is "host[:opt;opt;...]"; when an entry omits its
// options the shared nfsOptions (space-separated) apply. An empty clients list
// defaults to a single "*" rule with the shared options.
func parseNFSClients(clientsCSV, optionsStr string) []storagev1alpha1.NFSClient {
	defaultOpts := splitList(optionsStr, " ")
	entries := splitList(clientsCSV, ",")
	if len(entries) == 0 {
		return []storagev1alpha1.NFSClient{{Client: "*", Options: defaultOpts}}
	}

	var out []storagev1alpha1.NFSClient
	for _, e := range entries {
		host := e
		opts := defaultOpts
		if i := strings.IndexByte(e, ':'); i >= 0 {
			host = strings.TrimSpace(e[:i])
			opts = splitList(e[i+1:], ";")
		}
		if host == "" {
			continue
		}
		out = append(out, storagev1alpha1.NFSClient{Client: host, Options: opts})
	}
	if len(out) == 0 {
		return []storagev1alpha1.NFSClient{{Client: "*", Options: defaultOpts}}
	}
	return out
}

// splitList splits s on sep, trims whitespace, and drops empty fields. It
// returns nil (not an empty slice) when nothing remains.
func splitList(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}
