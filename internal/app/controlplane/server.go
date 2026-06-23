package controlplane

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/control"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/rpcauth"
	"google.golang.org/grpc"
)

type Server struct {
	addr           string
	listener       net.Listener
	grpc           *grpc.Server
	conn           *grpc.ClientConn
	meta           io.Closer
	checkpointStop func()
}

type Options struct {
	MetadataStore               string
	PostgresDSN                 string
	PostgresMode                string
	APIToken                    string
	MetadataCheckpointInterval  time.Duration
	MetadataCheckpointRetention int
}

func Start(addr, logAddr string) (*Server, error) {
	return StartWithOptions(addr, logAddr, Options{
		MetadataStore:               os.Getenv("LOGSERVE_METADATA_STORE"),
		PostgresDSN:                 firstNonEmpty(os.Getenv("LOGSERVE_POSTGRES_DSN"), os.Getenv("DATABASE_URL")),
		PostgresMode:                os.Getenv("LOGSERVE_POSTGRES_MODE"),
		APIToken:                    os.Getenv(rpcauth.EnvAPIToken),
		MetadataCheckpointInterval:  durationFromEnvMs("LOGSERVE_METADATA_CHECKPOINT_INTERVAL_MS", 0),
		MetadataCheckpointRetention: intFromEnv("LOGSERVE_METADATA_CHECKPOINT_RETENTION", 3),
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
	if opts.MetadataCheckpointRetention == 0 {
		opts.MetadataCheckpointRetention = 3
	}
	if err := service.LogBootstrapResult(service.BootstrapFromLog(context.Background())); err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	checkpointStop := service.StartMetadataCheckpointLoop(context.Background(), opts.MetadataCheckpointInterval, opts.MetadataCheckpointRetention)
	logservepb.RegisterControlServiceServer(grpcServer, service)
	srv := &Server{addr: lis.Addr().String(), listener: lis, grpc: grpcServer, conn: conn, meta: closer, checkpointStop: checkpointStop}
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	return srv, nil
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) Stop() error {
	if s.checkpointStop != nil {
		s.checkpointStop()
	}
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
		store, err := metadata.OpenPostgresStoreWithOptions(ctx, opts.PostgresDSN, metadata.PostgresOptions{Mode: metadata.PostgresWriteMode(opts.PostgresMode)})
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

func durationFromEnvMs(key string, fallbackMs int) time.Duration {
	value := intFromEnv(key, fallbackMs)
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

func intFromEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
