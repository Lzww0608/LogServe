package logd

import (
	"net"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logstore"
	"google.golang.org/grpc"
)

type Server struct {
	addr     string
	listener net.Listener
	grpc     *grpc.Server
	store    *logstore.Store
}

func Start(addr, dataDir string) (*Server, error) {
	store, err := logstore.Open(dataDir)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	grpcServer := grpc.NewServer()
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
