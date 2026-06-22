package controlplane

import (
	"context"
	"io"
	"net"
	"os"
	"strings"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/control"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/rpcauth"
	"google.golang.org/grpc"
)

type Server struct {
	addr     string
	listener net.Listener
	grpc     *grpc.Server
	conn     *grpc.ClientConn
	meta     io.Closer
}

type Options struct {
	MetadataStore string
	PostgresDSN   string
	APIToken      string
}

func Start(addr, logAddr string) (*Server, error) {
	return StartWithOptions(addr, logAddr, Options{
		MetadataStore: os.Getenv("LOGSERVE_METADATA_STORE"),
		PostgresDSN:   firstNonEmpty(os.Getenv("LOGSERVE_POSTGRES_DSN"), os.Getenv("DATABASE_URL")),
		APIToken:      os.Getenv(rpcauth.EnvAPIToken),
	})
}

func StartWithOptions(addr, logAddr string, opts Options) (*Server, error) {
	apiToken := firstNonEmpty(opts.APIToken, os.Getenv(rpcauth.EnvAPIToken))
	conn, err := grpc.NewClient(logAddr, rpcauth.InsecureDialOptions(apiToken)...)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	meta, closer, err := openMetadataStore(context.Background(), opts)
	if err != nil {
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	store, err := objectstore.OpenFromEnv(context.Background())
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	grpcServer := grpc.NewServer(rpcauth.ServerOptions(apiToken)...)
	service := control.NewServiceWithResultStore(meta, logservepb.NewLogServiceClient(conn), store, 0)
	if err := service.LogBootstrapResult(service.BootstrapFromLog(context.Background())); err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	logservepb.RegisterControlServiceServer(grpcServer, service)
	srv := &Server{addr: lis.Addr().String(), listener: lis, grpc: grpcServer, conn: conn, meta: closer}
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
	if s.meta != nil {
		_ = s.meta.Close()
	}
	return s.conn.Close()
}

func openMetadataStore(ctx context.Context, opts Options) (metadata.Store, io.Closer, error) {
	switch strings.ToLower(strings.TrimSpace(opts.MetadataStore)) {
	case "", "memory":
		return metadata.NewMemoryStore(), nil, nil
	case "postgres", "postgresql":
		store, err := metadata.OpenPostgresStore(ctx, opts.PostgresDSN)
		if err != nil {
			return nil, nil, err
		}
		return store, store, nil
	default:
		return nil, nil, os.ErrInvalid
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
