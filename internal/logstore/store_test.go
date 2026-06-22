package logstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"google.golang.org/grpc"
)

func TestAppendReadAndIdempotency(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rec, duplicate, err := store.Append(AppendRequest{
		StreamID:       "task:1",
		EventType:      "TaskSubmitted",
		IdempotencyKey: "submit-1",
		Payload:        []byte(`{"x":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first append reported duplicate")
	}
	if rec.Seq != 1 {
		t.Fatalf("seq = %d, want 1", rec.Seq)
	}

	again, duplicate, err := store.Append(AppendRequest{
		StreamID:       "task:1",
		EventType:      "TaskSubmitted",
		IdempotencyKey: "submit-1",
		Payload:        []byte(`{"x":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate {
		t.Fatal("second append should be duplicate")
	}
	if again.Seq != rec.Seq {
		t.Fatalf("duplicate seq = %d, want %d", again.Seq, rec.Seq)
	}

	records, err := store.Read("task:1", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
}

func TestReadLogStreamReadsAllWhenLimitUnset(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 150; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:streaming-read",
			EventType: "TaskSubmitted",
			Payload:   []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	service := NewService(store)
	unary, err := service.ReadLog(context.Background(), &logservepb.ReadLogRequest{StreamId: "task:streaming-read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unary.GetRecords()) != 100 {
		t.Fatalf("unary records = %d, want default 100", len(unary.GetRecords()))
	}

	stream := &captureReadLogStream{ctx: context.Background()}
	if err := service.ReadLogStream(&logservepb.ReadLogRequest{StreamId: "task:streaming-read"}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.records) != 150 {
		t.Fatalf("stream records = %d, want 150", len(stream.records))
	}
	for i, rec := range stream.records {
		if rec.GetSeq() != uint64(i+1) {
			t.Fatalf("stream record[%d].Seq = %d, want %d", i, rec.GetSeq(), i+1)
		}
	}
}

type captureReadLogStream struct {
	grpc.ServerStream
	ctx     context.Context
	records []*logservepb.LogRecord
}

func (s *captureReadLogStream) Context() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

func (s *captureReadLogStream) Send(rec *logservepb.LogRecord) error {
	s.records = append(s.records, rec)
	return nil
}
func TestRecoveryTruncatesPartialTail(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(AppendRequest{
		StreamID:  "task:2",
		EventType: "TaskSubmitted",
		Payload:   []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "segment-00000001.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("task:2", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
}

func TestSegmentRollingRecoverAndReadAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.SegmentSizeBytes = 220
	opts.FsyncPolicy = FsyncAlways

	store, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 128)
	for i := 0; i < 12; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:       "wf:rolling",
			EventType:      "StepCompleted",
			IdempotencyKey: fmt.Sprintf("step-%02d", i),
			Payload:        payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	logSegments := globSegments(t, dir, ".log")
	if len(logSegments) < 2 {
		t.Fatalf("log segments = %d, want at least 2", len(logSegments))
	}

	recovered, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("wf:rolling", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 12 {
		t.Fatalf("records = %d, want 12", len(records))
	}
	for i, rec := range records {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record[%d].Seq = %d, want %d", i, rec.Seq, i+1)
		}
		if !bytes.Equal(rec.Payload, payload) {
			t.Fatalf("record[%d] payload changed", i)
		}
	}
}

func TestSegmentReaderCacheReusesEvictsAndCloses(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.SegmentSizeBytes = 220
	opts.SegmentReaderCacheSize = 2
	opts.FsyncPolicy = FsyncAlways

	store, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 128)
	for i := 0; i < 12; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "wf:reader-cache",
			EventType: "StepCompleted",
			Payload:   payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(globSegments(t, dir, ".log")) < 3 {
		t.Fatalf("test setup did not create enough segments")
	}

	for i := 0; i < 3; i++ {
		records, err := store.Read("wf:reader-cache", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 12 {
			t.Fatalf("records = %d, want 12", len(records))
		}
		if got := store.cachedSegmentReaderCount(); got > opts.SegmentReaderCacheSize {
			t.Fatalf("cached segment readers = %d, want <= %d", got, opts.SegmentReaderCacheSize)
		}
	}
	if got := store.cachedSegmentReaderCount(); got == 0 {
		t.Fatal("expected segment reader cache to retain at least one fd after reads")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := store.cachedSegmentReaderCount(); got != 0 {
		t.Fatalf("cached segment readers after close = %d, want 0", got)
	}
}
func TestIndexRebuiltFromSegments(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.SegmentSizeBytes = 220
	opts.FsyncPolicy = FsyncAlways

	store, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:index-rebuild",
			EventType: "TaskSubmitted",
			Payload:   bytes.Repeat([]byte{byte('a' + i)}, 128),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	logSegments := globSegments(t, dir, ".log")
	for _, path := range globSegments(t, dir, ".index") {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	recovered, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("task:index-rebuild", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("records = %d, want 6", len(records))
	}
	indexSegments := globSegments(t, dir, ".index")
	if len(indexSegments) != len(logSegments) {
		t.Fatalf("index segments = %d, want %d", len(indexSegments), len(logSegments))
	}
}

func TestLogicalTrimFiltersReadsAndReportsCompactableBytes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 64)
	for i := 0; i < 5; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "actor:trim",
			EventType: "ActorCommandApplied",
			Payload:   payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.Trim("actor:trim", 4)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TrimmedBeforeSeq != 4 {
		t.Fatalf("trimmed_before_seq = %d, want 4", stats.TrimmedBeforeSeq)
	}
	if stats.CompactableRecords != 3 {
		t.Fatalf("compactable_records = %d, want 3", stats.CompactableRecords)
	}
	if stats.CompactableBytes == 0 {
		t.Fatal("compactable bytes should be > 0")
	}

	records, err := store.Read("actor:trim", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Seq != 4 || records[1].Seq != 5 {
		t.Fatalf("visible seqs = %d,%d want 4,5", records[0].Seq, records[1].Seq)
	}

	statsList := store.Stats("actor:trim", "")
	if len(statsList) != 1 {
		t.Fatalf("stats streams = %d, want 1", len(statsList))
	}
	if statsList[0].CompactableRecords != 3 || statsList[0].CompactableBytes != stats.CompactableBytes {
		t.Fatalf("stats = %+v, want compactable records/bytes from trim response %+v", statsList[0], stats)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	records, err = recovered.Read("actor:trim", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Seq != 4 {
		t.Fatalf("recovered visible records = %+v, want seq >= 4", records)
	}
	if recovered.Stats("actor:trim", "")[0].TrimmedBeforeSeq != 4 {
		t.Fatalf("trim point was not recovered")
	}
}

func TestFsyncPoliciesAppendAndRecover(t *testing.T) {
	for _, policy := range []FsyncPolicy{FsyncBatch, FsyncInterval} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			opts := DefaultOptions()
			opts.FsyncPolicy = policy
			opts.FsyncInterval = time.Millisecond

			store, err := OpenWithOptions(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Append(AppendRequest{
				StreamID:  "task:fsync",
				EventType: "TaskSubmitted",
				Payload:   []byte(`{"ok":true}`),
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			recovered, err := OpenWithOptions(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			records, err := recovered.Read("task:fsync", 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
		})
	}
}

func TestOpenRejectsInvalidFsyncPolicy(t *testing.T) {
	opts := DefaultOptions()
	opts.FsyncPolicy = "sometimes"
	if _, err := OpenWithOptions(t.TempDir(), opts); err == nil {
		t.Fatal("OpenWithOptions accepted invalid fsync policy")
	}
}

func TestBinaryIndexReaderAndIdempotencyRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, duplicate, err := store.Append(AppendRequest{
		StreamID:       "task:binary-index",
		EventType:      "TaskSubmitted",
		IdempotencyKey: "submit-once",
		Payload:        []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first append reported duplicate")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	items, legacy, err := readSegmentIndex(segmentPath(dir, 1, ".index"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		t.Fatal("new index file was parsed as legacy JSON")
	}
	if len(items) != 1 || items[0].streamID != "task:binary-index" || items[0].entry.Seq != rec.Seq {
		t.Fatalf("binary index items = %+v, want one seq %d", items, rec.Seq)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	again, duplicate, err := recovered.Append(AppendRequest{
		StreamID:       "task:binary-index",
		EventType:      "TaskSubmitted",
		IdempotencyKey: "submit-once",
		Payload:        []byte(`{"changed":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate {
		t.Fatal("idempotency key should survive index recovery")
	}
	if again.Seq != rec.Seq {
		t.Fatalf("duplicate seq = %d, want %d", again.Seq, rec.Seq)
	}
}

func TestSegmentDictionaryIndexUsesFixedEntries(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const records = 20
	for i := 0; i < records; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:fixed-entry-index",
			EventType: "TaskSubmitted",
			Payload:   []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	path := segmentPath(dir, 1, ".index")
	header := assertSegmentIndexHeader(t, path, 1, records)
	wantDictionaryBytes := uint64(indexDictionaryHeaderSize + len("task:fixed-entry-index"))
	if header.dictionaryBytes != wantDictionaryBytes {
		t.Fatalf("dictionary bytes = %d, want %d", header.dictionaryBytes, wantDictionaryBytes)
	}
	wantSize := int64(indexFileHeaderSize) + int64(wantDictionaryBytes) + int64(indexFixedEntrySize*records)
	if header.size != wantSize {
		t.Fatalf("index size = %d, want fixed dictionary size %d", header.size, wantSize)
	}
	legacySize := int64((legacyIndexRecordHeaderSize + len("task:fixed-entry-index") + 4) * records)
	if header.size >= legacySize {
		t.Fatalf("dictionary index size = %d, want smaller than legacy per-entry size %d", header.size, legacySize)
	}
}

func TestLegacyBinaryIndexRecoveredAndRewrittenAsDictionary(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:legacy-binary-index",
			EventType: "TaskSubmitted",
			Payload:   []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeLegacyBinaryIndex(t, dir, 1)

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	records, err := recovered.Read("task:legacy-binary-index", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	assertSegmentIndexHeader(t, segmentPath(dir, 1, ".index"), 1, 3)
}

func TestLegacyJSONIndexRecoveredAndRewrittenAsBinary(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:legacy-index",
			EventType: "TaskSubmitted",
			Payload:   []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteLegacyJSONIndex(t, dir, 1)

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	records, err := recovered.Read("task:legacy-index", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	assertSegmentIndexHeader(t, segmentPath(dir, 1, ".index"), 1, 3)
}

func TestCorruptBinaryIndexFallsBackToLogAndRebuilds(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:corrupt-index",
			EventType: "TaskSubmitted",
			Payload:   []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	indexPath := segmentPath(dir, 1, ".index")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 4 {
		t.Fatalf("index file too small: %d", len(data))
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(indexPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	records, err := recovered.Read("task:corrupt-index", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4", len(records))
	}
	assertSegmentIndexHeader(t, indexPath, 1, 4)
}

func rewriteLegacyJSONIndex(t *testing.T, dir string, segmentID uint64) {
	t.Helper()
	logFile, err := os.Open(segmentPath(dir, segmentID, ".log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	indexFile, err := os.OpenFile(segmentPath(dir, segmentID, ".index"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(indexFile)
	var offset int64
	for {
		rec, nextOffset, err := readRecordAt(logFile, offset)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(struct {
			StreamID  string `json:"stream_id"`
			Seq       uint64 `json:"seq"`
			SegmentID uint64 `json:"segment_id"`
			Offset    int64  `json:"offset"`
			Length    int64  `json:"length"`
		}{
			StreamID:  rec.StreamID,
			Seq:       rec.Seq,
			SegmentID: segmentID,
			Offset:    offset,
			Length:    nextOffset - offset,
		}); err != nil {
			t.Fatal(err)
		}
		offset = nextOffset
	}
	if err := indexFile.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyBinaryIndex(t *testing.T, dir string, segmentID uint64) {
	t.Helper()
	logFile, err := os.Open(segmentPath(dir, segmentID, ".log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	indexFile, err := os.OpenFile(segmentPath(dir, segmentID, ".index"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	var offset int64
	for {
		rec, nextOffset, err := readRecordAt(logFile, offset)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		length := nextOffset - offset
		if length <= 0 || length > int64(^uint32(0)) {
			t.Fatalf("record length = %d, cannot encode legacy index", length)
		}
		writeLegacyIndexEntry(t, indexFile, rec.StreamID, streamIndexEntry{
			Seq:       rec.Seq,
			SegmentID: segmentID,
			Offset:    uint64(offset),
			Length:    uint32(length),
		})
		offset = nextOffset
	}
	if err := indexFile.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyIndexEntry(t *testing.T, w io.Writer, streamID string, entry streamIndexEntry) {
	t.Helper()
	streamBytes := []byte(streamID)
	buf := make([]byte, legacyIndexRecordHeaderSize+len(streamBytes)+4)
	binary.BigEndian.PutUint32(buf[0:4], indexMagic)
	binary.BigEndian.PutUint16(buf[4:6], legacyIndexVersion)
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(streamBytes)))
	binary.BigEndian.PutUint64(buf[8:16], entry.Seq)
	binary.BigEndian.PutUint64(buf[16:24], entry.SegmentID)
	binary.BigEndian.PutUint64(buf[24:32], entry.Offset)
	binary.BigEndian.PutUint32(buf[32:36], entry.Length)
	copy(buf[legacyIndexRecordHeaderSize:], streamBytes)
	crc := crc32.Checksum(buf[:legacyIndexRecordHeaderSize+len(streamBytes)], indexCRCTable)
	binary.BigEndian.PutUint32(buf[legacyIndexRecordHeaderSize+len(streamBytes):], crc)
	if _, err := w.Write(buf); err != nil {
		t.Fatal(err)
	}
}

type segmentIndexHeaderForTest struct {
	version         uint16
	streamCount     uint32
	entryCount      uint64
	dictionaryBytes uint64
	size            int64
}

func assertSegmentIndexHeader(t *testing.T, path string, wantStreams uint32, wantEntries uint64) segmentIndexHeaderForTest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < indexFileHeaderSize {
		t.Fatalf("index file too small: %d", len(data))
	}
	if got := binary.BigEndian.Uint32(data[:4]); got != indexMagic {
		t.Fatalf("index magic = %#x, want %#x", got, indexMagic)
	}
	if got := binary.BigEndian.Uint16(data[4:6]); got != indexVersion {
		t.Fatalf("index version = %d, want %d", got, indexVersion)
	}
	if got := binary.BigEndian.Uint16(data[6:8]); got != indexFileHeaderSize {
		t.Fatalf("index header size = %d, want %d", got, indexFileHeaderSize)
	}
	header := segmentIndexHeaderForTest{
		version:         binary.BigEndian.Uint16(data[4:6]),
		streamCount:     binary.BigEndian.Uint32(data[16:20]),
		entryCount:      binary.BigEndian.Uint64(data[20:28]),
		dictionaryBytes: binary.BigEndian.Uint64(data[28:36]),
		size:            int64(len(data)),
	}
	if header.streamCount != wantStreams {
		t.Fatalf("stream count = %d, want %d", header.streamCount, wantStreams)
	}
	if header.entryCount != wantEntries {
		t.Fatalf("entry count = %d, want %d", header.entryCount, wantEntries)
	}
	return header
}

func globSegments(t *testing.T, dir, ext string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*"+ext))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
