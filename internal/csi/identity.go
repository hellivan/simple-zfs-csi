package csi

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// IdentityServer implements the CSI Identity service: plugin name/version,
// advertised capabilities, and liveness.
type IdentityServer struct {
	csi.UnimplementedIdentityServer

	DriverName string
	Version    string
	// Capabilities are the plugin-level capabilities advertised by
	// GetPluginCapabilities. The controller plugin sets CONTROLLER_SERVICE; the
	// node plugin leaves it empty (no controller service, no topology).
	Capabilities []*csi.PluginCapability
}

// GetPluginInfo returns the driver name and version.
func (s *IdentityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          s.DriverName,
		VendorVersion: s.Version,
	}, nil
}

// GetPluginCapabilities advertises the capabilities configured on the server.
// Topology/accessibility is intentionally not advertised: pools are selected by
// StorageClass, not by scheduler topology.
func (s *IdentityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{Capabilities: s.Capabilities}, nil
}

// ControllerServiceCapability returns the plugin capability advertising the
// CONTROLLER_SERVICE, for use by the controller plugin's IdentityServer.
func ControllerServiceCapability() *csi.PluginCapability {
	return &csi.PluginCapability{
		Type: &csi.PluginCapability_Service_{
			Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
			},
		},
	}
}

// Probe reports readiness.
func (s *IdentityServer) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
