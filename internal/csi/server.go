package csi

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
)

// Serve starts the CSI gRPC server on endpoint and blocks until ctx is cancelled
// or the server fails. endpoint is a URL such as "unix:///csi/csi.sock" or
// "tcp://127.0.0.1:10000".
func Serve(ctx context.Context, endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, log logr.Logger) error {
	network, addr, err := parseEndpoint(endpoint)
	if err != nil {
		return err
	}
	if network == "unix" {
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale socket %q: %w", addr, err)
		}
	}

	listener, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("listen on %s://%s: %w", network, addr, err)
	}

	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(logInterceptor(log)))
	csi.RegisterIdentityServer(srv, ids)
	csi.RegisterControllerServer(srv, cs)

	go func() {
		<-ctx.Done()
		log.Info("shutting down CSI server")
		srv.GracefulStop()
	}()

	log.Info("CSI server listening", "network", network, "address", addr)
	if err := srv.Serve(listener); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// parseEndpoint splits a CSI endpoint URL into a net.Listen network and address.
func parseEndpoint(endpoint string) (network, addr string, err error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		return "unix", strings.TrimPrefix(endpoint, "unix://"), nil
	case strings.HasPrefix(endpoint, "tcp://"):
		return "tcp", strings.TrimPrefix(endpoint, "tcp://"), nil
	case strings.HasPrefix(endpoint, "/"):
		return "unix", endpoint, nil
	default:
		return "", "", fmt.Errorf("invalid endpoint %q: want unix:// or tcp:// prefix", endpoint)
	}
}

// logInterceptor logs each gRPC call and any error at a low verbosity.
func logInterceptor(log logr.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			log.Error(err, "gRPC call failed", "method", info.FullMethod)
		} else {
			log.V(1).Info("gRPC call", "method", info.FullMethod)
		}
		return resp, err
	}
}
