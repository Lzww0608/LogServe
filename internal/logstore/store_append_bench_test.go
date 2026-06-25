package logstore

import (
	"bytes"
	"fmt"
	"testing"
)

func BenchmarkStoreAppendSingleStream(b *testing.B) {
	for _, payloadSize := range []int{128, 1024, 64 << 10} {
		for _, policy := range []FsyncPolicy{FsyncAlways, FsyncBatch, FsyncInterval} {
			b.Run(fmt.Sprintf("payload=%s/fsync=%s", payloadSizeName(payloadSize), policy), func(b *testing.B) {
				payload := bytes.Repeat([]byte("a"), payloadSize)
				opts := DefaultOptions()
				opts.FsyncPolicy = policy
				opts.SegmentSizeBytes = 1 << 30
				store, err := OpenWithOptions(b.TempDir(), opts)
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				b.SetBytes(int64(payloadSize))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := store.Append(AppendRequest{
						StreamID:  "bench:append-single",
						EventType: "BenchAppend",
						Payload:   payload,
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkStoreAppendMultiStream(b *testing.B) {
	for _, streams := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("streams=%d", streams), func(b *testing.B) {
			opts := DefaultOptions()
			opts.FsyncPolicy = FsyncBatch
			opts.SegmentSizeBytes = 1 << 30
			store, err := OpenWithOptions(b.TempDir(), opts)
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				streamID := fmt.Sprintf("bench:stream-%d", i%streams)
				if _, _, err := store.Append(AppendRequest{
					StreamID:  streamID,
					EventType: "BenchAppend",
					Payload:   []byte("x"),
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkStoreRecoverLargeSegments(b *testing.B) {
	for _, entries := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			dir := b.TempDir()
			opts := DefaultOptions()
			opts.FsyncPolicy = FsyncBatch
			opts.SegmentSizeBytes = 4096
			seed, err := OpenWithOptions(dir, opts)
			if err != nil {
				b.Fatal(err)
			}
			payload := bytes.Repeat([]byte("x"), 256)
			for i := 0; i < entries; i++ {
				if _, _, err := seed.Append(AppendRequest{
					StreamID:  "bench:recover",
					EventType: "BenchRecover",
					Payload:   payload,
				}); err != nil {
					b.Fatal(err)
				}
			}
			if err := seed.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store, err := OpenWithOptions(dir, opts)
				if err != nil {
					b.Fatal(err)
				}
				records, err := store.Read("bench:recover", 1, 16)
				if err != nil {
					store.Close()
					b.Fatal(err)
				}
				if len(records) != 16 {
					store.Close()
					b.Fatalf("records = %d, want 16", len(records))
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
