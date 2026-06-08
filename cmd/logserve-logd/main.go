package main

import (
	"flag"
	"log"

	"github.com/logserve/logserve/internal/app/logd"
	"github.com/logserve/logserve/internal/observability"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "gRPC listen address")
	dataDir := flag.String("data-dir", "data/logstore", "logstore data directory")
	flag.Parse()

	srv, err := logd.Start(*addr, *dataDir)
	if err != nil {
		log.Fatal(err)
	}
	observability.Info("logd_started", map[string]any{"addr": srv.Addr(), "data_dir": *dataDir})
	select {}
}
