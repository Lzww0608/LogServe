package main

import (
	"flag"
	"log"
	"time"

	"github.com/logserve/logserve/internal/app/logd"
	"github.com/logserve/logserve/internal/logstore"
	"github.com/logserve/logserve/internal/observability"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "gRPC listen address")
	dataDir := flag.String("data-dir", "data/logstore", "logstore data directory")
	segmentSizeBytes := flag.Int64("segment-size-bytes", logstore.DefaultOptions().SegmentSizeBytes, "maximum bytes per log segment before rolling")
	fsyncPolicy := flag.String("fsync-policy", string(logstore.FsyncAlways), "fsync policy: always, batch, interval")
	fsyncIntervalMs := flag.Int64("fsync-interval-ms", int64(logstore.DefaultOptions().FsyncInterval/time.Millisecond), "fsync interval in milliseconds when --fsync-policy=interval")
	flag.Parse()

	opts := logstore.DefaultOptions()
	opts.SegmentSizeBytes = *segmentSizeBytes
	opts.FsyncPolicy = logstore.FsyncPolicy(*fsyncPolicy)
	opts.FsyncInterval = time.Duration(*fsyncIntervalMs) * time.Millisecond

	srv, err := logd.StartWithOptions(*addr, *dataDir, opts)
	if err != nil {
		log.Fatal(err)
	}
	observability.Info("logd_started", map[string]any{
		"addr":               srv.Addr(),
		"data_dir":           *dataDir,
		"segment_size_bytes": opts.SegmentSizeBytes,
		"fsync_policy":       opts.FsyncPolicy,
		"fsync_interval_ms":  opts.FsyncInterval.Milliseconds(),
	})
	select {}
}
