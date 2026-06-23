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

func main() {
	addr := flag.String("addr", "127.0.0.1:50052", "gRPC listen address")
	logAddr := flag.String("log-addr", "127.0.0.1:50051", "log service address")
	metadataStore := flag.String("metadata-store", getenv("LOGSERVE_METADATA_STORE", "memory"), "metadata store: memory or postgres")
	postgresDSN := flag.String("postgres-dsn", getenv("LOGSERVE_POSTGRES_DSN", getenv("DATABASE_URL", "")), "PostgreSQL DSN when --metadata-store=postgres")
	postgresMode := flag.String("postgres-mode", getenv("LOGSERVE_POSTGRES_MODE", "sync"), "PostgreSQL write mode: sync or async")
	metadataCheckpointIntervalMs := flag.Int("metadata-checkpoint-interval-ms", getenvInt("LOGSERVE_METADATA_CHECKPOINT_INTERVAL_MS", 0), "metadata checkpoint interval in milliseconds; 0 disables periodic checkpoints")
	metadataCheckpointRetention := flag.Int("metadata-checkpoint-retention", getenvInt("LOGSERVE_METADATA_CHECKPOINT_RETENTION", 3), "metadata checkpoints to retain in system:checkpoints")
	apiToken := flag.String("api-token", getenv("LOGSERVE_API_TOKEN", ""), "API token required for gRPC calls when set")
	flag.Parse()

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
	select {}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

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
