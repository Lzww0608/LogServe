package main

import (
	"flag"
	"log"

	"github.com/logserve/logserve/internal/app/controlplane"
	"github.com/logserve/logserve/internal/observability"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50052", "gRPC listen address")
	logAddr := flag.String("log-addr", "127.0.0.1:50051", "log service address")
	flag.Parse()

	srv, err := controlplane.Start(*addr, *logAddr)
	if err != nil {
		log.Fatal(err)
	}
	observability.Info("control_started", map[string]any{"addr": srv.Addr(), "log_addr": *logAddr})
	select {}
}
