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
	"sync"
	"time"
)

const (
	magic      uint32 = 0x4c535647
	version    uint16 = 1
	headerSize        = 36
)

var errCorruptRecord = errors.New("corrupt log record")

type Store struct {
	mu          sync.Mutex
	dir         string
	logPath     string
	indexPath   string
	logFile     *os.File
	indexFile   *os.File
	nextSeq     map[string]uint64
	records     map[string][]Record
	idempotency map[string]Record
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	s := &Store{
		dir:         dir,
		logPath:     filepath.Join(dir, "segment-00000001.log"),
		indexPath:   filepath.Join(dir, "segment-00000001.index"),
		nextSeq:     make(map[string]uint64),
		records:     make(map[string][]Record),
		idempotency: make(map[string]Record),
	}

	if err := s.recover(); err != nil {
		return nil, err
	}

	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	indexFile, err := os.OpenFile(s.indexPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	s.logFile = logFile
	s.indexFile = indexFile
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	if s.logFile != nil {
		err = errors.Join(err, s.logFile.Sync(), s.logFile.Close())
		s.logFile = nil
	}
	if s.indexFile != nil {
		err = errors.Join(err, s.indexFile.Sync(), s.indexFile.Close())
		s.indexFile = nil
	}
	return err
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

	offset, err := s.logFile.Seek(0, io.SeekEnd)
	if err != nil {
		return Record{}, false, err
	}
	if _, err := s.logFile.Write(encoded); err != nil {
		return Record{}, false, err
	}
	if err := s.logFile.Sync(); err != nil {
		return Record{}, false, err
	}
	if err := s.appendIndex(rec, offset); err != nil {
		return Record{}, false, err
	}

	s.records[rec.StreamID] = append(s.records[rec.StreamID], cloneRecord(rec))
	s.nextSeq[rec.StreamID] = seq + 1
	if rec.IdempotencyKey != "" {
		s.idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = cloneRecord(rec)
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
	defer s.mu.Unlock()

	source := s.records[streamID]
	out := make([]Record, 0, min(limit, len(source)))
	for _, rec := range source {
		if rec.Seq < fromSeq {
			continue
		}
		out = append(out, cloneRecord(rec))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Store) recover() error {
	file, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
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
				return truncateErr
			}
			break
		}
		s.records[rec.StreamID] = append(s.records[rec.StreamID], cloneRecord(rec))
		if s.nextSeq[rec.StreamID] <= rec.Seq {
			s.nextSeq[rec.StreamID] = rec.Seq + 1
		}
		if rec.IdempotencyKey != "" {
			s.idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = cloneRecord(rec)
		}
		offset = nextOffset
	}
	return s.rewriteIndex()
}

func (s *Store) rewriteIndex() error {
	file, err := os.OpenFile(s.indexPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	logFile, err := os.Open(s.logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	var offset int64
	for {
		rec, nextOffset, err := readRecordAt(logFile, offset)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := writeIndexEntry(file, rec, offset); err != nil {
			return err
		}
		offset = nextOffset
	}
	return file.Sync()
}

func (s *Store) appendIndex(rec Record, offset int64) error {
	if err := writeIndexEntry(s.indexFile, rec, offset); err != nil {
		return err
	}
	return s.indexFile.Sync()
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

func writeIndexEntry(w io.Writer, rec Record, offset int64) error {
	entry := struct {
		StreamID string `json:"stream_id"`
		Seq      uint64 `json:"seq"`
		Offset   int64  `json:"offset"`
	}{
		StreamID: rec.StreamID,
		Seq:      rec.Seq,
		Offset:   offset,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return err
	}
	return nil
}

func cloneRecord(rec Record) Record {
	rec.Payload = append([]byte(nil), rec.Payload...)
	return rec
}

func idempotencyKey(streamID, key string) string {
	return streamID + "\x00" + key
}
