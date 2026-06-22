package logstore

import (
	"fmt"
	"testing"
)

func BenchmarkReadLargeStream(b *testing.B) {
	for _, entries := range []int{100_000, 1_000_000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			store := buildBenchmarkStore(b, entries)
			defer store.Close()

			cases := []struct {
				name    string
				fromSeq uint64
			}{
				{name: "from=head", fromSeq: 1},
				{name: "from=middle", fromSeq: uint64(entries / 2)},
				{name: "from=tail", fromSeq: uint64(entries - 9)},
			}
			for _, tc := range cases {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						records, err := store.Read("bench:large-stream", tc.fromSeq, 10)
						if err != nil {
							b.Fatal(err)
						}
						if len(records) != 10 {
							b.Fatalf("records = %d, want 10", len(records))
						}
					}
				})
			}
		})
	}
}

func buildBenchmarkStore(b *testing.B, entries int) *Store {
	b.Helper()
	opts := DefaultOptions()
	opts.FsyncPolicy = FsyncBatch
	opts.SegmentSizeBytes = 1 << 30
	store, err := OpenWithOptions(b.TempDir(), opts)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < entries; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "bench:large-stream",
			EventType: "BenchEvent",
			Payload:   []byte("x"),
		}); err != nil {
			b.Fatal(err)
		}
	}
	return store
}
func BenchmarkReadAcrossSegments(b *testing.B) {
	for _, cacheSize := range []int{0, 64} {
		b.Run(fmt.Sprintf("cache=%d", cacheSize), func(b *testing.B) {
			store := buildCrossSegmentBenchmarkStore(b, cacheSize)
			defer store.Close()

			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				records, err := store.Read("bench:cross-segment", 1, 48)
				if err != nil {
					b.Fatal(err)
				}
				if len(records) != 48 {
					b.Fatalf("records = %d, want 48", len(records))
				}
			}
		})
	}
}

func buildCrossSegmentBenchmarkStore(b *testing.B, cacheSize int) *Store {
	b.Helper()
	opts := DefaultOptions()
	opts.FsyncPolicy = FsyncBatch
	opts.SegmentSizeBytes = 260
	opts.SegmentReaderCacheSize = cacheSize
	store, err := OpenWithOptions(b.TempDir(), opts)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = 'x'
	}
	for i := 0; i < 48; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "bench:cross-segment",
			EventType: "BenchEvent",
			Payload:   payload,
		}); err != nil {
			b.Fatal(err)
		}
	}
	return store
}
