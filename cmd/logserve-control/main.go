package main

import (
	"flag"
	"log"
	"os"

	"github.com/logserve/logserve/internal/app/controlplane"
	"github.com/logserve/logserve/internal/observability"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50052", "gRPC listen address")
	logAddr := flag.String("log-addr", "127.0.0.1:50051", "log service address")
	metadataStore := flag.String("metadata-store", getenv("LOGSERVE_METADATA_STORE", "memory"), "metadata store: memory or postgres")
	postgresDSN := flag.String("postgres-dsn", getenv("LOGSERVE_POSTGRES_DSN", getenv("DATABASE_URL", "")), "PostgreSQL DSN when --metadata-store=postgres")
	flag.Parse()

	srv, err := controlplane.StartWithOptions(*addr, *logAddr, controlplane.Options{
		MetadataStore: *metadataStore,
		PostgresDSN:   *postgresDSN,
	})
	if err != nil {
		log.Fatal(err)
	}
	observability.Info("control_started", map[string]any{"addr": srv.Addr(), "log_addr": *logAddr, "metadata_store": *metadataStore})
	select {}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
