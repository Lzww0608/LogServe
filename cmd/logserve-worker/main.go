package main

import (
	"context"
	"flag"
	"log"
	"strings"
	"time"

	"github.com/logserve/logserve/internal/worker"
)

func main() {
	cfg := worker.Config{}
	flag.StringVar(&cfg.WorkerID, "worker-id", "worker-1", "worker id")
	flag.StringVar(&cfg.ControlAddr, "control-addr", "127.0.0.1:50052", "control service address")
	flag.StringVar(&cfg.LogAddr, "log-addr", "127.0.0.1:50051", "log service address")
	flag.StringVar(&cfg.PythonPath, "python", "python", "python executable")
	flag.StringVar(&cfg.ExecutorPath, "executor", "executor/python/server.py", "python executor path")
	models := flag.String("models", "", "comma-separated cached models, for example model-A:v1,model-B:v1")
	capacity := flag.Uint("capacity", 1, "worker task capacity")
	flag.StringVar(&cfg.VLLMBaseURL, "vllm-base-url", "", "vLLM OpenAI-compatible base URL")
	pollMs := flag.Int("poll-ms", 200, "poll interval in milliseconds")
	flag.IntVar(&cfg.MaxTasks, "max-tasks", 0, "optional max tasks before the worker exits")
	flag.Parse()
	cfg.PollInterval = time.Duration(*pollMs) * time.Millisecond
	cfg.CachedModels = splitCSV(*models)
	cfg.Capacity = uint32(*capacity)

	if err := worker.Run(context.Background(), cfg); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
