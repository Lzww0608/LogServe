package logstore

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/logserve/logserve/internal/logrecord"
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

func BenchmarkReadRawLargeStream(b *testing.B) {
	store := buildBenchmarkStore(b, 100_000)
	defer store.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		if err := store.ReadRawEach("bench:large-stream", 50_000, 10, func(rec logrecord.RawRecord) error {
			if len(rec.Payload) != 1 {
				b.Fatalf("payload len = %d", len(rec.Payload))
			}
			count++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if count != 10 {
			b.Fatalf("records = %d, want 10", count)
		}
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

func BenchmarkChecksumPayloadSizes(b *testing.B) {
	for _, size := range []int{128, 1024, 64 << 10, 1 << 20} {
		payload := bytes.Repeat([]byte("x"), size)
		for _, typ := range []ChecksumType{ChecksumTypeIEEE, ChecksumTypeCRC32C, ChecksumTypeXXH3, ChecksumTypeNone} {
			b.Run(fmt.Sprintf("%s/%s", payloadSizeName(size), typ), func(b *testing.B) {
				b.SetBytes(int64(size))
				b.ReportAllocs()
				var sum uint32
				for i := 0; i < b.N; i++ {
					got, err := checksum(payload, typ)
					if err != nil {
						b.Fatal(err)
					}
					sum = got
				}
				_ = sum
			})
		}
	}
}

func BenchmarkEncodeRecordPayloadSizes(b *testing.B) {
	for _, size := range []int{128, 1024, 64 << 10, 1 << 20} {
		payload := bytes.Repeat([]byte("x"), size)
		rec := Record{
			StreamID:       "bench:checksum",
			Seq:            1,
			EventType:      "ChecksumEvent",
			IdempotencyKey: "bench-key",
			Payload:        payload,
			TimestampMs:    1,
		}
		for _, typ := range []ChecksumType{ChecksumTypeIEEE, ChecksumTypeCRC32C, ChecksumTypeXXH3} {
			b.Run(fmt.Sprintf("%s/%s/allocated", payloadSizeName(size), typ), func(b *testing.B) {
				b.SetBytes(int64(size))
				b.ReportAllocs()
				var encoded []byte
				for i := 0; i < b.N; i++ {
					buf, _, err := encodeRecord(rec, typ)
					if err != nil {
						b.Fatal(err)
					}
					encoded = buf
				}
				_ = encoded
			})
			b.Run(fmt.Sprintf("%s/%s/pooled", payloadSizeName(size), typ), func(b *testing.B) {
				b.SetBytes(int64(size))
				b.ReportAllocs()
				var encodedLen int
				for i := 0; i < b.N; i++ {
					buf, _, err := encodeRecordPooled(rec, typ)
					if err != nil {
						b.Fatal(err)
					}
					encodedLen = len(buf)
					putRecordEncodeBuffer(buf)
				}
				_ = encodedLen
			})
		}
	}
}
func payloadSizeName(size int) string {
	switch size {
	case 128:
		return "128B"
	case 1024:
		return "1KB"
	case 64 << 10:
		return "64KB"
	case 1 << 20:
		return "1MB"
	default:
		return fmt.Sprintf("%dB", size)
	}
}
