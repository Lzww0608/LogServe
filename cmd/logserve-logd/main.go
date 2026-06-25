package main

import (
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/logserve/logserve/internal/app/logd"
	"github.com/logserve/logserve/internal/logstore"
	"github.com/logserve/logserve/internal/observability"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "gRPC listen address")
	pprofAddr := flag.String("pprof-addr", observability.PprofAddrFromEnv(), "optional pprof listen address, for example 127.0.0.1:6061")
	dataDir := flag.String("data-dir", "data/logstore", "logstore data directory")
	segmentSizeBytes := flag.Int64("segment-size-bytes", logstore.DefaultOptions().SegmentSizeBytes, "maximum bytes per log segment before rolling")
	fsyncPolicy := flag.String("fsync-policy", string(logstore.FsyncAlways), "fsync policy: always, batch, interval")
	fsyncIntervalMs := flag.Int64("fsync-interval-ms", int64(logstore.DefaultOptions().FsyncInterval/time.Millisecond), "fsync interval in milliseconds when --fsync-policy=interval")
	compactionIntervalMs := flag.Int64("compaction-interval-ms", int64(logstore.DefaultOptions().CompactionInterval/time.Millisecond), "physical compaction interval in milliseconds; 0 disables background compaction")
	compactionCopyLiveRatio := flag.Float64("compaction-copy-live-ratio", logstore.DefaultOptions().CompactionCopyLiveRatioThreshold, "copy compact sealed segments when live_bytes/total_bytes is at or below this ratio")
	compactionMaxBytesPerSecond := flag.Int64("compaction-max-bytes-per-second", logstore.DefaultOptions().CompactionMaxBytesPerSecond, "maximum copy-compaction write rate in bytes per second")
	flag.Parse()

	observability.StartDebugServer(*pprofAddr)

	opts := logstore.DefaultOptions()
	opts.SegmentSizeBytes = *segmentSizeBytes
	opts.FsyncPolicy = logstore.FsyncPolicy(*fsyncPolicy)
	opts.FsyncInterval = time.Duration(*fsyncIntervalMs) * time.Millisecond
	opts.CompactionInterval = time.Duration(*compactionIntervalMs) * time.Millisecond
	opts.CompactionCopyLiveRatioThreshold = *compactionCopyLiveRatio
	opts.CompactionMaxBytesPerSecond = *compactionMaxBytesPerSecond
	if v := os.Getenv("LOGSERVE_LOG_MMAP_READ"); v == "1" || strings.EqualFold(v, "true") {
		opts.MmapRead = true
	}

	srv, err := logd.StartWithOptions(*addr, *dataDir, opts)
	if err != nil {
		log.Fatal(err)
	}
	observability.Info("logd_started", map[string]any{
		"addr":                            srv.Addr(),
		"data_dir":                        *dataDir,
		"segment_size_bytes":              opts.SegmentSizeBytes,
		"fsync_policy":                    opts.FsyncPolicy,
		"fsync_interval_ms":               opts.FsyncInterval.Milliseconds(),
		"compaction_interval_ms":          opts.CompactionInterval.Milliseconds(),
		"compaction_copy_live_ratio":      opts.CompactionCopyLiveRatioThreshold,
		"compaction_max_bytes_per_second": opts.CompactionMaxBytesPerSecond,
		"mmap_read":                       opts.MmapRead,
	})
	select {}
}
