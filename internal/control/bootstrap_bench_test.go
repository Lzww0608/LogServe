package control

import (
	"context"
	"fmt"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

// seedBootstrapTaskStreams creates synthetic task streams for bootstrap benchmarks.
func seedBootstrapTaskStreams(b *testing.B, logClient *countingReplayableLogClient, streams, recordsPerStream int) {
	b.Helper()
	spec := &logservepb.TaskSpec{
		TaskName:       "bench-task",
		FunctionName:   "bench_fn",
		FunctionSource: "def bench_fn():\n    return 1\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
	}
	payload, err := protojson.Marshal(spec)
	if err != nil {
		b.Fatal(err)
	}
	for s := 0; s < streams; s++ {
		streamID := fmt.Sprintf("task:bench-%d", s)
		for r := 0; r < recordsPerStream; r++ {
			if _, err := logClient.AppendLog(context.Background(), &logservepb.AppendLogRequest{
				StreamId:       streamID,
				EventType:      "TaskSubmitted",
				IdempotencyKey: fmt.Sprintf("%s:%d", streamID, r),
				Payload:        payload,
			}); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkBootstrapFromLog measures full control-plane bootstrap over synthetic
// task stream counts and records-per-stream depths.
func BenchmarkBootstrapFromLog(b *testing.B) {
	for _, streams := range []int{10, 100} {
		for _, records := range []int{1, 4} {
			b.Run(fmt.Sprintf("streams=%d/records=%d", streams, records), func(b *testing.B) {
				seedClient := newCountingReplayableLogClient()
				seedBootstrapTaskStreams(b, seedClient, streams, records)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					logClient := newCountingReplayableLogClient()
					for streamID, records := range seedClient.records {
						cloned := make([]*logservepb.LogRecord, len(records))
						copy(cloned, records)
						logClient.records[streamID] = cloned
					}
					service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
					b.StartTimer()
					if err := service.BootstrapFromLog(context.Background()); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
