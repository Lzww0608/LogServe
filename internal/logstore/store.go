package logstore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	magic                   uint32 = 0x4c535647
	version                 uint16 = 1
	headerSize                     = 36
	defaultSegmentSizeBytes int64  = 64 << 20
	defaultFsyncInterval           = 100 * time.Millisecond
	retentionFileName              = "retention.json"
)

var errCorruptRecord = errors.New("corrupt log record")

type FsyncPolicy string

const (
	FsyncAlways   FsyncPolicy = "always"
	FsyncBatch    FsyncPolicy = "batch"
	FsyncInterval FsyncPolicy = "interval"
)

type Options struct {
	SegmentSizeBytes int64
	FsyncPolicy      FsyncPolicy
	FsyncInterval    time.Duration
}

func DefaultOptions() Options {
	return Options{
		SegmentSizeBytes: defaultSegmentSizeBytes,
		FsyncPolicy:      FsyncAlways,
		FsyncInterval:    defaultFsyncInterval,
	}
}

func (opts Options) normalize() (Options, error) {
	if opts.SegmentSizeBytes <= 0 {
		opts.SegmentSizeBytes = defaultSegmentSizeBytes
	}
	if opts.FsyncPolicy == "" {
		opts.FsyncPolicy = FsyncAlways
	}
	if opts.FsyncInterval <= 0 {
		opts.FsyncInterval = defaultFsyncInterval
	}
	switch opts.FsyncPolicy {
	case FsyncAlways, FsyncBatch, FsyncInterval:
		return opts, nil
	default:
		return Options{}, fmt.Errorf("unsupported fsync policy %q", opts.FsyncPolicy)
	}
}

type Store struct {
	mu                 sync.Mutex
	dir                string
	options            Options
	logFile            *os.File
	indexFile          *os.File
	activeSegmentID    uint64
	activeSegmentBytes int64
	nextSeq            map[string]uint64
	index              map[string][]indexEntry
	idempotency        map[string]Record
	trimBefore         map[string]uint64
	lastSync           time.Time
}

type indexEntry struct {
	StreamID  string
	Seq       uint64
	SegmentID uint64
	Offset    int64
	Length    int64
}

type retentionFile struct {
	Streams map[string]retentionStream `json:"streams"`
}

type retentionStream struct {
	TrimmedBeforeSeq uint64 `json:"trimmed_before_seq"`
	UpdatedAtMs      int64  `json:"updated_at_ms"`
}

func Open(dir string) (*Store, error) {
	return OpenWithOptions(dir, DefaultOptions())
}

func OpenWithOptions(dir string, opts Options) (*Store, error) {
	normalized, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	s := &Store{
		dir:         dir,
		options:     normalized,
		nextSeq:     make(map[string]uint64),
		index:       make(map[string][]indexEntry),
		idempotency: make(map[string]Record),
		trimBefore:  make(map[string]uint64),
	}

	if err := s.recover(); err != nil {
		return nil, err
	}
	if err := s.loadRetention(); err != nil {
		return nil, err
	}
	if err := s.openActiveFilesLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closeActiveFilesLocked(true)
}

func (s *Store) Append(req AppendRequest) (Record, bool, error) {
	if req.StreamID == "" {
		return Record{}, false, errors.New("stream_id is required")
	}
	if req.EventType == "" {
		return Record{}, false, errors.New("event_type is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.IdempotencyKey != "" {
		if existing, ok := s.idempotency[idempotencyKey(req.StreamID, req.IdempotencyKey)]; ok {
			return cloneRecord(existing), true, nil
		}
	}

	seq := s.nextSeq[req.StreamID]
	if seq == 0 {
		seq = 1
	}
	rec := Record{
		StreamID:       req.StreamID,
		Seq:            seq,
		EventType:      req.EventType,
		IdempotencyKey: req.IdempotencyKey,
		Payload:        append([]byte(nil), req.Payload...),
		TimestampMs:    time.Now().UnixMilli(),
	}

	encoded, crc, err := encodeRecord(rec)
	if err != nil {
		return Record{}, false, err
	}
	rec.CRC32 = crc
	if err := s.ensureWritableSegmentLocked(int64(len(encoded))); err != nil {
		return Record{}, false, err
	}

	offset := s.activeSegmentBytes
	if _, err := s.logFile.Write(encoded); err != nil {
		return Record{}, false, err
	}
	entry := indexEntry{
		StreamID:  rec.StreamID,
		Seq:       rec.Seq,
		SegmentID: s.activeSegmentID,
		Offset:    offset,
		Length:    int64(len(encoded)),
	}
	if err := s.appendIndex(entry); err != nil {
		return Record{}, false, err
	}
	if err := s.syncForPolicyLocked(); err != nil {
		return Record{}, false, err
	}

	s.activeSegmentBytes += int64(len(encoded))
	s.index[rec.StreamID] = append(s.index[rec.StreamID], entry)
	s.nextSeq[rec.StreamID] = seq + 1
	if rec.IdempotencyKey != "" {
		s.idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = idempotencyRecord(rec)
	}
	return rec, false, nil
}

func (s *Store) Read(streamID string, fromSeq uint64, limit int) ([]Record, error) {
	if streamID == "" {
		return nil, errors.New("stream_id is required")
	}
	if fromSeq == 0 {
		fromSeq = 1
	}
	if limit <= 0 {
		limit = 100
	}

	s.mu.Lock()
	if trimmedBefore := s.trimBefore[streamID]; trimmedBefore > fromSeq {
		fromSeq = trimmedBefore
	}
	entries := s.index[streamID]
	selected := make([]indexEntry, 0, min(limit, len(entries)))
	for _, entry := range entries {
		if entry.Seq < fromSeq {
			continue
		}
		selected = append(selected, entry)
		if len(selected) == limit {
			break
		}
	}
	s.mu.Unlock()

	out := make([]Record, 0, len(selected))
	var (
		currentSegmentID uint64
		currentFile      *os.File
	)
	defer func() {
		if currentFile != nil {
			_ = currentFile.Close()
		}
	}()
	for _, entry := range selected {
		if currentFile == nil || currentSegmentID != entry.SegmentID {
			if currentFile != nil {
				_ = currentFile.Close()
			}
			file, err := os.Open(segmentPath(s.dir, entry.SegmentID, ".log"))
			if err != nil {
				return nil, err
			}
			currentFile = file
			currentSegmentID = entry.SegmentID
		}
		rec, err := readIndexedRecordFromFile(currentFile, entry)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *Store) Trim(streamID string, beforeSeq uint64) (TrimStats, error) {
	if streamID == "" {
		return TrimStats{}, errors.New("stream_id is required")
	}
	if beforeSeq == 0 {
		return TrimStats{}, errors.New("before_seq is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.index[streamID]; !ok && s.nextSeq[streamID] == 0 {
		return TrimStats{}, fmt.Errorf("stream %q not found", streamID)
	}
	nextSeq := s.nextSeq[streamID]
	if nextSeq == 0 {
		nextSeq = 1
	}
	if beforeSeq > nextSeq {
		beforeSeq = nextSeq
	}
	if beforeSeq < 1 {
		beforeSeq = 1
	}
	if beforeSeq > s.trimBefore[streamID] {
		s.trimBefore[streamID] = beforeSeq
		if err := s.persistRetentionLocked(); err != nil {
			return TrimStats{}, err
		}
	}
	return s.streamStatsLocked(streamID), nil
}

func (s *Store) Stats(streamID, prefix string) []TrimStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	streams := make(map[string]bool)
	for id := range s.index {
		streams[id] = true
	}
	for id := range s.trimBefore {
		streams[id] = true
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		if streamID != "" && id != streamID {
			continue
		}
		if prefix != "" && !strings.HasPrefix(id, prefix) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]TrimStats, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.streamStatsLocked(id))
	}
	return out
}

func (s *Store) ListStreams(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.index))
	for streamID := range s.index {
		if prefix == "" || strings.HasPrefix(streamID, prefix) {
			out = append(out, streamID)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) recover() error {
	segmentIDs, err := discoverSegmentIDs(s.dir, ".log")
	if err != nil {
		return err
	}
	if len(segmentIDs) == 0 {
		s.activeSegmentID = 1
		s.activeSegmentBytes = 0
		return s.rewriteIndex()
	}

	for _, segmentID := range segmentIDs {
		size, err := s.recoverSegment(segmentID)
		if err != nil {
			return err
		}
		s.activeSegmentID = segmentID
		s.activeSegmentBytes = size
	}
	return s.rewriteIndex()
}

func (s *Store) loadRetention() error {
	path := filepath.Join(s.dir, retentionFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file retentionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	for streamID, meta := range file.Streams {
		if streamID == "" || meta.TrimmedBeforeSeq == 0 {
			continue
		}
		s.trimBefore[streamID] = meta.TrimmedBeforeSeq
	}
	return nil
}

func (s *Store) persistRetentionLocked() error {
	file := retentionFile{Streams: make(map[string]retentionStream, len(s.trimBefore))}
	now := time.Now().UnixMilli()
	for streamID, beforeSeq := range s.trimBefore {
		if beforeSeq == 0 {
			continue
		}
		file.Streams[streamID] = retentionStream{
			TrimmedBeforeSeq: beforeSeq,
			UpdatedAtMs:      now,
		}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, retentionFileName)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (s *Store) streamStatsLocked(streamID string) TrimStats {
	entries := s.index[streamID]
	trimmedBefore := s.trimBefore[streamID]
	nextSeq := s.nextSeq[streamID]
	if nextSeq == 0 {
		nextSeq = 1
	}
	stats := TrimStats{
		StreamID:         streamID,
		NextSeq:          nextSeq,
		TrimmedBeforeSeq: trimmedBefore,
	}
	for _, entry := range entries {
		if trimmedBefore > 0 && entry.Seq < trimmedBefore {
			stats.CompactableRecords++
			stats.CompactableBytes += uint64(entry.Length)
			continue
		}
		if stats.FirstSeq == 0 || entry.Seq < stats.FirstSeq {
			stats.FirstSeq = entry.Seq
		}
	}
	if stats.FirstSeq == 0 {
		stats.FirstSeq = nextSeq
	}
	return stats
}

func (s *Store) recoverSegment(segmentID uint64) (int64, error) {
	path := segmentPath(s.dir, segmentID, ".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var offset int64
	for {
		rec, nextOffset, err := readRecordAt(file, offset)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if truncateErr := file.Truncate(offset); truncateErr != nil {
				return 0, truncateErr
			}
			break
		}
		entry := indexEntry{
			StreamID:  rec.StreamID,
			Seq:       rec.Seq,
			SegmentID: segmentID,
			Offset:    offset,
			Length:    nextOffset - offset,
		}
		s.index[rec.StreamID] = append(s.index[rec.StreamID], entry)
		if s.nextSeq[rec.StreamID] <= rec.Seq {
			s.nextSeq[rec.StreamID] = rec.Seq + 1
		}
		if rec.IdempotencyKey != "" {
			s.idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = idempotencyRecord(rec)
		}
		offset = nextOffset
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *Store) rewriteIndex() error {
	indexIDs, err := discoverSegmentIDs(s.dir, ".index")
	if err != nil {
		return err
	}
	for _, segmentID := range indexIDs {
		if err := os.Remove(segmentPath(s.dir, segmentID, ".index")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	segmentIDs, err := discoverSegmentIDs(s.dir, ".log")
	if err != nil {
		return err
	}
	if len(segmentIDs) == 0 && s.activeSegmentID != 0 {
		segmentIDs = []uint64{s.activeSegmentID}
	}

	bySegment := make(map[uint64][]indexEntry)
	for _, entries := range s.index {
		for _, entry := range entries {
			bySegment[entry.SegmentID] = append(bySegment[entry.SegmentID], entry)
		}
	}

	for _, segmentID := range segmentIDs {
		entries := bySegment[segmentID]
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Offset < entries[j].Offset
		})
		file, err := os.OpenFile(segmentPath(s.dir, segmentID, ".index"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := writeIndexEntry(file, entry); err != nil {
				_ = file.Close()
				return err
			}
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendIndex(entry indexEntry) error {
	return writeIndexEntry(s.indexFile, entry)
}

func readIndexedRecordFromFile(file *os.File, entry indexEntry) (Record, error) {
	rec, nextOffset, err := readRecordAt(file, entry.Offset)
	if err != nil {
		return Record{}, err
	}
	if rec.StreamID != entry.StreamID || rec.Seq != entry.Seq || nextOffset-entry.Offset != entry.Length {
		return Record{}, errCorruptRecord
	}
	return cloneRecord(rec), nil
}

func (s *Store) ensureWritableSegmentLocked(recordLen int64) error {
	if s.logFile == nil || s.indexFile == nil {
		return errors.New("logstore is closed")
	}
	if s.activeSegmentBytes > 0 && s.activeSegmentBytes+recordLen > s.options.SegmentSizeBytes {
		return s.rollSegmentLocked()
	}
	return nil
}

func (s *Store) rollSegmentLocked() error {
	if err := s.closeActiveFilesLocked(true); err != nil {
		return err
	}
	s.activeSegmentID++
	s.activeSegmentBytes = 0
	return s.openActiveFilesLocked()
}

func (s *Store) openActiveFilesLocked() error {
	if s.activeSegmentID == 0 {
		s.activeSegmentID = 1
	}
	logFile, err := os.OpenFile(segmentPath(s.dir, s.activeSegmentID, ".log"), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	indexFile, err := os.OpenFile(segmentPath(s.dir, s.activeSegmentID, ".index"), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		_ = logFile.Close()
		return err
	}
	info, err := logFile.Stat()
	if err != nil {
		_ = logFile.Close()
		_ = indexFile.Close()
		return err
	}
	s.logFile = logFile
	s.indexFile = indexFile
	s.activeSegmentBytes = info.Size()
	s.lastSync = time.Now()
	return nil
}

func (s *Store) closeActiveFilesLocked(sync bool) error {
	var err error
	if sync {
		err = errors.Join(err, s.syncFilesLocked())
	}
	if s.logFile != nil {
		err = errors.Join(err, s.logFile.Close())
		s.logFile = nil
	}
	if s.indexFile != nil {
		err = errors.Join(err, s.indexFile.Close())
		s.indexFile = nil
	}
	return err
}

func (s *Store) syncForPolicyLocked() error {
	switch s.options.FsyncPolicy {
	case FsyncAlways:
		return s.syncFilesLocked()
	case FsyncBatch:
		return nil
	case FsyncInterval:
		if time.Since(s.lastSync) >= s.options.FsyncInterval {
			return s.syncFilesLocked()
		}
		return nil
	default:
		return fmt.Errorf("unsupported fsync policy %q", s.options.FsyncPolicy)
	}
}

func (s *Store) syncFilesLocked() error {
	var err error
	if s.logFile != nil {
		err = errors.Join(err, s.logFile.Sync())
	}
	if s.indexFile != nil {
		err = errors.Join(err, s.indexFile.Sync())
	}
	if err == nil {
		s.lastSync = time.Now()
	}
	return err
}

func encodeRecord(rec Record) ([]byte, uint32, error) {
	if len(rec.StreamID) > 0xffff {
		return nil, 0, errors.New("stream_id too large")
	}
	if len(rec.EventType) > 0xffff {
		return nil, 0, errors.New("event_type too large")
	}
	if len(rec.IdempotencyKey) > 0xffff {
		return nil, 0, errors.New("idempotency_key too large")
	}

	stream := []byte(rec.StreamID)
	eventType := []byte(rec.EventType)
	key := []byte(rec.IdempotencyKey)
	bodyLen := len(stream) + len(eventType) + len(key) + len(rec.Payload)
	buf := make([]byte, headerSize+bodyLen)

	binary.BigEndian.PutUint32(buf[0:4], magic)
	binary.BigEndian.PutUint16(buf[4:6], version)
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(stream)))
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(eventType)))
	binary.BigEndian.PutUint16(buf[10:12], uint16(len(key)))
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(rec.Payload)))
	binary.BigEndian.PutUint64(buf[16:24], rec.Seq)
	binary.BigEndian.PutUint64(buf[24:32], uint64(rec.TimestampMs))

	pos := headerSize
	pos += copy(buf[pos:], stream)
	pos += copy(buf[pos:], eventType)
	pos += copy(buf[pos:], key)
	copy(buf[pos:], rec.Payload)

	crc := crc32.ChecksumIEEE(buf[headerSize:])
	binary.BigEndian.PutUint32(buf[32:36], crc)
	return buf, crc, nil
}

func readRecordAt(file *os.File, offset int64) (Record, int64, error) {
	header := make([]byte, headerSize)
	n, err := file.ReadAt(header, offset)
	if errors.Is(err, io.EOF) && n == 0 {
		return Record{}, offset, io.EOF
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return Record{}, offset, err
	}
	if n != headerSize {
		return Record{}, offset, errCorruptRecord
	}

	if binary.BigEndian.Uint32(header[0:4]) != magic {
		return Record{}, offset, errCorruptRecord
	}
	if binary.BigEndian.Uint16(header[4:6]) != version {
		return Record{}, offset, errCorruptRecord
	}

	streamLen := int(binary.BigEndian.Uint16(header[6:8]))
	eventLen := int(binary.BigEndian.Uint16(header[8:10]))
	keyLen := int(binary.BigEndian.Uint16(header[10:12]))
	payloadLen := int(binary.BigEndian.Uint32(header[12:16]))
	seq := binary.BigEndian.Uint64(header[16:24])
	timestampMs := int64(binary.BigEndian.Uint64(header[24:32]))
	expectedCRC := binary.BigEndian.Uint32(header[32:36])

	bodyLen := streamLen + eventLen + keyLen + payloadLen
	body := make([]byte, bodyLen)
	n, err = file.ReadAt(body, offset+headerSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return Record{}, offset, err
	}
	if n != bodyLen {
		return Record{}, offset, errCorruptRecord
	}
	if crc32.ChecksumIEEE(body) != expectedCRC {
		return Record{}, offset, errCorruptRecord
	}

	pos := 0
	streamID := string(body[pos : pos+streamLen])
	pos += streamLen
	eventType := string(body[pos : pos+eventLen])
	pos += eventLen
	key := string(body[pos : pos+keyLen])
	pos += keyLen
	payload := append([]byte(nil), body[pos:pos+payloadLen]...)

	return Record{
		StreamID:       streamID,
		Seq:            seq,
		EventType:      eventType,
		IdempotencyKey: key,
		Payload:        payload,
		TimestampMs:    timestampMs,
		CRC32:          expectedCRC,
	}, offset + int64(headerSize+bodyLen), nil
}

func writeIndexEntry(w io.Writer, entry indexEntry) error {
	data, err := json.Marshal(struct {
		StreamID  string `json:"stream_id"`
		Seq       uint64 `json:"seq"`
		SegmentID uint64 `json:"segment_id"`
		Offset    int64  `json:"offset"`
		Length    int64  `json:"length"`
	}{
		StreamID:  entry.StreamID,
		Seq:       entry.Seq,
		SegmentID: entry.SegmentID,
		Offset:    entry.Offset,
		Length:    entry.Length,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return err
	}
	return nil
}

func segmentPath(dir string, segmentID uint64, ext string) string {
	return filepath.Join(dir, fmt.Sprintf("segment-%08d%s", segmentID, ext))
}

func discoverSegmentIDs(dir, ext string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "segment-") || !strings.HasSuffix(name, ext) {
			continue
		}
		rawID := strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ext)
		id, err := strconv.ParseUint(rawID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse segment id from %q: %w", name, err)
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i] < out[j]
	})
	return out, nil
}

func cloneRecord(rec Record) Record {
	rec.Payload = append([]byte(nil), rec.Payload...)
	return rec
}

func idempotencyRecord(rec Record) Record {
	rec.Payload = nil
	return rec
}

func idempotencyKey(streamID, key string) string {
	return streamID + "\x00" + key
}
