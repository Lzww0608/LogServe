package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/webapi"
)

func main() {
	defaults := webapi.DefaultConfig()
	addr := flag.String("addr", defaults.Addr, "HTTP listen address")
	controlAddr := flag.String("control-addr", defaults.ControlAddr, "control service gRPC address")
	logAddr := flag.String("log-addr", defaults.LogAddr, "log service gRPC address")
	apiTokenEnv := flag.String("api-token-env", "LOGSERVE_API_TOKEN", "environment variable containing the API token")
	staticDir := flag.String("static-dir", defaults.StaticDir, "static frontend directory")
	devCORS := flag.Bool("dev-cors", defaults.DevCORS, "allow development CORS requests")
	allowUnauthenticated := flag.Bool("allow-unauthenticated", defaults.AllowUnauthenticated, "allow API access without a bearer token; unsafe outside local development")
	requestTimeoutMs := flag.Int("request-timeout-ms", int(defaults.RequestTimeout/time.Millisecond), "per-request backend timeout in milliseconds")
	flag.Parse()

	cfg := webapi.Config{
		Addr:                 *addr,
		ControlAddr:          *controlAddr,
		LogAddr:              *logAddr,
		APIToken:             os.Getenv(*apiTokenEnv),
		StaticDir:            *staticDir,
		DevCORS:              *devCORS,
		AllowUnauthenticated: *allowUnauthenticated,
		RequestTimeout:       time.Duration(*requestTimeoutMs) * time.Millisecond,
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if envAddr := os.Getenv("LOGSERVE_WEB_ADDR"); envAddr != "" && !flagVisited("addr") {
		cfg.Addr = envAddr
	}
	if envValue := os.Getenv("LOGSERVE_WEB_DEV_CORS"); envValue != "" && !flagVisited("dev-cors") {
		if parsed, err := strconv.ParseBool(envValue); err == nil {
			cfg.DevCORS = parsed
		}
	}
	if envValue := os.Getenv("LOGSERVE_WEB_ALLOW_UNAUTHENTICATED"); envValue != "" && !flagVisited("allow-unauthenticated") {
		if parsed, err := strconv.ParseBool(envValue); err == nil {
			cfg.AllowUnauthenticated = parsed
		}
	}

	srv, err := webapi.NewServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	observability.Info("web_started", map[string]any{
		"addr":         cfg.Addr,
		"control_addr": cfg.ControlAddr,
		"log_addr":     cfg.LogAddr,
		"static_dir":   cfg.StaticDir,
		"dev_cors":     cfg.DevCORS,
	})
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func flagVisited(name string) bool {
	visited := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			visited = true
		}
	})
	return visited
}
