package main

import (
	"context"
	"flag"
	"log"
	"strings"
	"time"

	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/worker"
)

func main() {
	cfg := worker.Config{}
	pprofAddr := flag.String("pprof-addr", observability.PprofAddrFromEnv(), "optional pprof listen address, for example 127.0.0.1:6063")
	flag.StringVar(&cfg.WorkerID, "worker-id", "worker-1", "worker id")
	flag.StringVar(&cfg.ControlAddr, "control-addr", "127.0.0.1:50052", "control service address")
	flag.StringVar(&cfg.LogAddr, "log-addr", "127.0.0.1:50051", "log service address")
	flag.StringVar(&cfg.PythonPath, "python", "python", "python executable")
	flag.StringVar(&cfg.ExecutorPath, "executor", "executor/python/server.py", "python executor path")
	models := flag.String("models", "", "comma-separated cached models, for example model-A:v1,model-B:v1")
	capacity := flag.Uint("capacity", 1, "worker task capacity")
	flag.IntVar(&cfg.TaskPoolSize, "task-pool-size", 0, "local Python task executor pool size; 0 follows capacity")
	flag.IntVar(&cfg.LLMPoolSize, "llm-pool-size", 0, "local LLM executor pool size; 0 follows capacity")
	flag.IntVar(&cfg.ActorPoolSize, "actor-pool-size", 0, "local actor executor pool size; per-actor ordering is still enforced")
	flag.StringVar(&cfg.VLLMBaseURL, "vllm-base-url", "", "vLLM OpenAI-compatible base URL")
	flag.StringVar(&cfg.ModelCheckpointSourceDir, "model-source-dir", "", "source directory for mock model checkpoints")
	flag.StringVar(&cfg.ModelCacheDir, "model-cache-dir", "", "local directory used as the mock model checkpoint cache")
	flag.Int64Var(&cfg.ModelCacheCapacityBytes, "model-cache-capacity-bytes", 0, "optional local model cache capacity in bytes; 0 means unlimited")
	pollMs := flag.Int("poll-ms", 200, "poll/long-poll timeout in milliseconds")
	heartbeatMs := flag.Int("heartbeat-ms", 1000, "heartbeat interval in milliseconds")
	flag.IntVar(&cfg.MaxTasks, "max-tasks", 0, "optional max tasks before the worker exits")
	flag.Parse()
	observability.StartDebugServer(*pprofAddr)
	cfg.PollInterval = time.Duration(*pollMs) * time.Millisecond
	cfg.HeartbeatInterval = time.Duration(*heartbeatMs) * time.Millisecond
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
