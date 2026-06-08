package controlplane

import (
	"context"
	"net"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/control"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Server struct {
	addr     string
	listener net.Listener
	grpc     *grpc.Server
	conn     *grpc.ClientConn
}

func Start(addr, logAddr string) (*Server, error) {
	conn, err := grpc.NewClient(logAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	store, err := objectstore.OpenFromEnv(context.Background())
	if err != nil {
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	grpcServer := grpc.NewServer()
	service := control.NewServiceWithResultStore(metadata.NewMemoryStore(), logservepb.NewLogServiceClient(conn), store, 0)
	if err := service.LogBootstrapResult(service.BootstrapFromLog(context.Background())); err != nil {
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	logservepb.RegisterControlServiceServer(grpcServer, service)
	srv := &Server{addr: lis.Addr().String(), listener: lis, grpc: grpcServer, conn: conn}
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
	return s.conn.Close()
}
