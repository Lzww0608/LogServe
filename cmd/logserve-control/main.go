// Command logserve-control starts the control-plane gRPC service and wires it
// to a running log service plus the selected metadata store.
package main

import (
	"flag"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/logserve/logserve/internal/app/controlplane"
	"github.com/logserve/logserve/internal/observability"
)

// main parses startup flags, starts optional debug endpoints, and launches
// the control-plane service until the process is externally stopped.
func main() {
	addr := flag.String("addr", "127.0.0.1:50052", "gRPC listen address")
	pprofAddr := flag.String("pprof-addr", observability.PprofAddrFromEnv(), "optional pprof listen address, for example 127.0.0.1:6062")
	logAddr := flag.String("log-addr", "127.0.0.1:50051", "log service address")
	metadataStore := flag.String("metadata-store", getenv("LOGSERVE_METADATA_STORE", "memory"), "metadata store: memory or postgres")
	postgresDSN := flag.String("postgres-dsn", getenv("LOGSERVE_POSTGRES_DSN", getenv("DATABASE_URL", "")), "PostgreSQL DSN when --metadata-store=postgres")
	postgresMode := flag.String("postgres-mode", getenv("LOGSERVE_POSTGRES_MODE", "sync"), "PostgreSQL write mode: sync or async")
	metadataCheckpointIntervalMs := flag.Int("metadata-checkpoint-interval-ms", getenvInt("LOGSERVE_METADATA_CHECKPOINT_INTERVAL_MS", 0), "metadata checkpoint interval in milliseconds; 0 disables periodic checkpoints")
	metadataCheckpointRetention := flag.Int("metadata-checkpoint-retention", getenvInt("LOGSERVE_METADATA_CHECKPOINT_RETENTION", 3), "metadata checkpoints to retain in system:checkpoints")
	apiToken := flag.String("api-token", getenv("LOGSERVE_API_TOKEN", ""), "API token required for gRPC calls when set")
	flag.Parse()

	// The debug server is optional; an empty pprof address leaves the process
	// without the extra HTTP listener.
	observability.StartDebugServer(*pprofAddr)

	srv, err := controlplane.StartWithOptions(*addr, *logAddr, controlplane.Options{
		MetadataStore:               *metadataStore,
		PostgresDSN:                 *postgresDSN,
		PostgresMode:                *postgresMode,
		APIToken:                    *apiToken,
		MetadataCheckpointInterval:  time.Duration(*metadataCheckpointIntervalMs) * time.Millisecond,
		MetadataCheckpointRetention: *metadataCheckpointRetention,
	})
	if err != nil {
		log.Fatal(err)
	}
	observability.Info("control_started", map[string]any{
		"addr":                            srv.Addr(),
		"log_addr":                        *logAddr,
		"metadata_store":                  *metadataStore,
		"postgres_mode":                   *postgresMode,
		"metadata_checkpoint_interval_ms": *metadataCheckpointIntervalMs,
		"metadata_checkpoint_retention":   *metadataCheckpointRetention,
	})
	// StartWithOptions owns the serving goroutine. Keep the command alive until
	// the process is interrupted by its supervisor or the OS.
	select {}
}

// getenv keeps environment defaults close to the flag declarations without
// making empty variables override the command's built-in defaults.
func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// getenvInt intentionally falls back on malformed input so a bad optional
// tuning variable does not prevent the control plane from starting.
func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
