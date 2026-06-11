package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/logserve/logserve/internal/app/controlplane"
	"github.com/logserve/logserve/internal/app/logd"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/worker"
)

func main() {
	logAddr := flag.String("log-addr", "127.0.0.1:50051", "log service address")
	controlAddr := flag.String("control-addr", "127.0.0.1:50052", "control service address")
	dataDir := flag.String("data-dir", "data/logstore", "logstore data directory")
	workerID := flag.String("worker-id", "worker-1", "worker id")
	pythonPath := flag.String("python", "python", "python executable")
	executorPath := flag.String("executor", filepath.Join("executor", "python", "server.py"), "python executor path")
	models := flag.String("models", "", "comma-separated cached models, for example model-A:v1")
	capacity := flag.Uint("capacity", 1, "worker task capacity")
	taskPoolSize := flag.Int("task-pool-size", 0, "local Python task executor pool size; 0 follows capacity")
	llmPoolSize := flag.Int("llm-pool-size", 0, "local LLM executor pool size; 0 follows capacity")
	actorPoolSize := flag.Int("actor-pool-size", 0, "local actor executor pool size; per-actor ordering is still enforced")
	vllmBaseURL := flag.String("vllm-base-url", "", "vLLM OpenAI-compatible base URL")
	modelSourceDir := flag.String("model-source-dir", "", "source directory for mock model checkpoints")
	modelCacheDir := flag.String("model-cache-dir", "", "local directory used as the mock model checkpoint cache")
	modelCacheCapacityBytes := flag.Int64("model-cache-capacity-bytes", 0, "optional local model cache capacity in bytes; 0 means unlimited")
	flag.Parse()

	logServer, err := logd.Start(*logAddr, *dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer logServer.Stop()
	controlServer, err := controlplane.Start(*controlAddr, logServer.Addr())
	if err != nil {
		log.Fatal(err)
	}
	defer controlServer.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		err := worker.Run(ctx, worker.Config{
			WorkerID:                 *workerID,
			ControlAddr:              controlServer.Addr(),
			LogAddr:                  logServer.Addr(),
			PythonPath:               *pythonPath,
			ExecutorPath:             *executorPath,
			PollInterval:             100 * time.Millisecond,
			CachedModels:             splitCSV(*models),
			Capacity:                 uint32(*capacity),
			TaskPoolSize:             *taskPoolSize,
			LLMPoolSize:              *llmPoolSize,
			ActorPoolSize:            *actorPoolSize,
			VLLMBaseURL:              *vllmBaseURL,
			ModelCheckpointSourceDir: *modelSourceDir,
			ModelCacheDir:            *modelCacheDir,
			ModelCacheCapacityBytes:  *modelCacheCapacityBytes,
		})
		if err != nil && err != context.Canceled {
			observability.Error("dev_worker_stopped", err, map[string]any{"worker_id": *workerID})
			stop()
		}
	}()

	observability.Info("dev_started", map[string]any{
		"log_addr":     logServer.Addr(),
		"control_addr": controlServer.Addr(),
		"worker_id":    *workerID,
		"data_dir":     *dataDir,
	})
	<-ctx.Done()
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
