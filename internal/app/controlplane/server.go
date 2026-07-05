// Package controlplane wires the control service to its log-service client,
// metadata store, result object store, and process-level gRPC listener.
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

// Server owns all process-level resources for a control-plane instance. Stop
// shuts down the checkpoint loop, gRPC server, metadata store, and log client.
type Server struct {
	// addr records the effective bind address after net.Listen resolves
	// wildcards or port 0 into the listener's concrete address.
	addr string
	// listener is owned by grpc.Server.Serve and closed during GracefulStop.
	listener net.Listener
	// grpc exposes the control-plane RPC surface to workers, web, and CLI clients.
	grpc *grpc.Server
	// conn is the downstream client connection to logd; bootstrap and all log writes
	// flow through this process-owned connection.
	conn *grpc.ClientConn
	// meta closes durable metadata backends. It is nil for the in-memory store.
	meta io.Closer
	// checkpointStop cancels and joins the optional metadata checkpoint goroutine.
	checkpointStop func()
}

// Options contains startup-time control-plane dependencies and persistence
// settings. Empty fields keep the same environment/default behavior as Start.
type Options struct {
	// MetadataStore selects the metadata backend: empty and "memory" use an
	// in-process store; "postgres" and "postgresql" open the durable SQL store.
	MetadataStore string
	// PostgresDSN is required only when MetadataStore selects Postgres.
	PostgresDSN string
	// PostgresMode is forwarded to metadata.PostgresOptions and normalized there.
	PostgresMode string
	// APIToken overrides the shared environment token for both server auth and the
	// downstream log-service client; empty values fall back to the environment.
	APIToken string
	// MetadataCheckpointInterval controls the periodic checkpoint loop. Zero
	// disables the loop and still returns a no-op stopper.
	MetadataCheckpointInterval time.Duration
	// MetadataCheckpointRetention caps retained checkpoint records when positive.
	// Zero means unspecified and is normalized to the process default; negative
	// values pass through and therefore disable trimming in control checkpointing.
	MetadataCheckpointRetention int
}

// Start builds Options from environment variables and delegates to
// StartWithOptions. It is the production-friendly entrypoint used by commands
// that do not need to override individual dependencies in tests or dev tools.
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

// StartWithOptions connects to logd, opens metadata and object stores,
// bootstraps metadata from the log, and starts the ControlService gRPC server.
// The returned server must be stopped to release background loops and sockets.
func StartWithOptions(addr, logAddr string, opts Options) (*Server, error) {
	// Options take precedence, but the shared token environment variable remains
	// a fallback so command wrappers can pass partially populated Options.
	apiToken := firstNonEmpty(opts.APIToken, os.Getenv(rpcauth.EnvAPIToken))
	conn, err := grpc.NewClient(logAddr, rpcauth.InsecureDialOptions(apiToken)...)
	if err != nil {
		return nil, err
	}
	// The log client is configured before binding this service because the control
	// plane is not useful without a log-service endpoint. grpc.NewClient is lazy;
	// BootstrapFromLog below is the first log RPC that proves the endpoint works.
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Metadata is opened before the public server starts so startup either has a
	// usable state backend or fails without accepting requests.
	meta, closer, err := openMetadataStore(context.Background(), opts)
	if err != nil {
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	// The object store backs large results and registered function bodies; it is
	// intentionally selected from environment to match the control service.
	store, err := objectstore.OpenFromEnv(context.Background())
	if err != nil {
		// Object-store setup happens before the listener is published; failures
		// unwind every resource opened so far and leave no partially serving process.
		if closer != nil {
			_ = closer.Close()
		}
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	grpcServer := grpc.NewServer(rpcauth.ServerOptions(apiToken)...)
	service := control.NewServiceWithResultStore(meta, logservepb.NewLogServiceClient(conn), store, 0)
	// Retention zero means unspecified here rather than keep no checkpoints.
	if opts.MetadataCheckpointRetention == 0 {
		opts.MetadataCheckpointRetention = 3
	}
	// Bootstrap replays existing log history before serving new control-plane RPCs
	// so in-memory state is aligned with durable records at startup.
	if err := service.LogBootstrapResult(service.BootstrapFromLog(context.Background())); err != nil {
		// A failed replay leaves the service state ambiguous, so fail startup
		// before registering RPC handlers rather than accepting fresh mutations.
		if closer != nil {
			_ = closer.Close()
		}
		_ = conn.Close()
		_ = lis.Close()
		return nil, err
	}
	// The checkpoint loop is optional when interval is zero; the returned stopper
	// is still stored so Stop can release it uniformly.
	checkpointStop := service.StartMetadataCheckpointLoop(context.Background(), opts.MetadataCheckpointInterval, opts.MetadataCheckpointRetention)
	logservepb.RegisterControlServiceServer(grpcServer, service)
	srv := &Server{addr: lis.Addr().String(), listener: lis, grpc: grpcServer, conn: conn, meta: closer, checkpointStop: checkpointStop}
	// Serving happens asynchronously; callers use Addr to discover the bound port
	// and Stop to coordinate graceful shutdown.
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	return srv, nil
}

// Addr returns the actual listen address, including an ephemeral port chosen
// by the kernel when addr used port 0.
func (s *Server) Addr() string {
	return s.addr
}

// Stop halts background checkpointing, drains the gRPC server, closes metadata,
// and finally closes the downstream log-service client connection.
func (s *Server) Stop() error {
	if s.checkpointStop != nil {
		s.checkpointStop()
	}
	s.grpc.GracefulStop()
	if s.meta != nil {
		// Keep Stop's returned error tied to the log client close while still
		// attempting metadata cleanup for durable stores.
		_ = s.meta.Close()
	}
	return s.conn.Close()
}

// openMetadataStore selects the metadata backend requested at startup. The
// returned closer is nil for in-memory metadata and must be closed for durable
// stores such as Postgres.
func openMetadataStore(ctx context.Context, opts Options) (metadata.Store, io.Closer, error) {
	switch strings.ToLower(strings.TrimSpace(opts.MetadataStore)) {
	case "", "memory":
		return metadata.NewMemoryStore(), nil, nil
	case "postgres", "postgresql":
		// The metadata package normalizes empty or unknown modes to synchronous
		// writes, so this layer only passes through the startup string.
		store, err := metadata.OpenPostgresStoreWithOptions(ctx, opts.PostgresDSN, metadata.PostgresOptions{Mode: metadata.PostgresWriteMode(opts.PostgresMode)})
		if err != nil {
			return nil, nil, err
		}
		return store, store, nil
	default:
		return nil, nil, os.ErrInvalid
	}
}

// firstNonEmpty treats whitespace-only values as unset, which keeps empty
// environment variables from overriding configured defaults.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// durationFromEnvMs converts millisecond environment settings; non-positive
// values disable periodic behavior such as metadata checkpointing.
func durationFromEnvMs(key string, fallbackMs int) time.Duration {
	value := intFromEnv(key, fallbackMs)
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

// intFromEnv falls back on empty or malformed input so optional tuning
// variables do not prevent the service from starting.
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
