// Package logd provides the process-level wrapper that opens a logstore and
// exposes it through the generated LogService gRPC server.
package logd

import (
	"net"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logstore"
	"github.com/logserve/logserve/internal/rpcauth"
	"google.golang.org/grpc"
)

// Server owns the logstore, TCP listener, and gRPC server for one logd process.
// Call Stop to stop accepting RPCs and close the underlying store.
type Server struct {
	addr     string
	listener net.Listener
	grpc     *grpc.Server
	store    *logstore.Store
}

// Start opens logd with the repository's default logstore options.
func Start(addr, dataDir string) (*Server, error) {
	return StartWithOptions(addr, dataDir, logstore.DefaultOptions())
}

// StartWithOptions opens the durable store, binds the listener, registers the
// gRPC service, and starts serving in the background. If binding fails after
// the store opens, the store is closed before the error is returned.
func StartWithOptions(addr, dataDir string, opts logstore.Options) (*Server, error) {
	store, err := logstore.OpenWithOptions(dataDir, opts)
	if err != nil {
		return nil, err
	}
	// Bind after opening the store so startup fails early on corrupt or invalid
	// data directories, while still cleaning the store up if the port is busy.
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	grpcServer := grpc.NewServer(rpcauth.ServerOptionsFromEnv()...)
	logservepb.RegisterLogServiceServer(grpcServer, logstore.NewService(store))
	srv := &Server{addr: lis.Addr().String(), listener: lis, grpc: grpcServer, store: store}
	// Serve returns when Stop gracefully shuts the server down or when the listener
	// is closed by the process; startup only reports bind/open errors.
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	return srv, nil
}

// Addr returns the actual listen address, including the kernel-selected port
// when StartWithOptions was called with port 0.
func (s *Server) Addr() string {
	return s.addr
}

// Stop drains in-flight RPCs before closing the logstore.
func (s *Server) Stop() error {
	s.grpc.GracefulStop()
	return s.store.Close()
}
