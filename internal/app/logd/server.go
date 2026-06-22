package logd

import (
	"net"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logstore"
	"github.com/logserve/logserve/internal/rpcauth"
	"google.golang.org/grpc"
)

type Server struct {
	addr     string
	listener net.Listener
	grpc     *grpc.Server
	store    *logstore.Store
}

func Start(addr, dataDir string) (*Server, error) {
	return StartWithOptions(addr, dataDir, logstore.DefaultOptions())
}

func StartWithOptions(addr, dataDir string, opts logstore.Options) (*Server, error) {
	store, err := logstore.OpenWithOptions(dataDir, opts)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	grpcServer := grpc.NewServer(rpcauth.ServerOptionsFromEnv()...)
	logservepb.RegisterLogServiceServer(grpcServer, logstore.NewService(store))
	srv := &Server{addr: lis.Addr().String(), listener: lis, grpc: grpcServer, store: store}
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	return srv, nil
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) Stop() error {
	s.grpc.GracefulStop()
	return s.store.Close()
}
