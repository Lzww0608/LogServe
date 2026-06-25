package logstore

import (
	"bufio"
	"bytes"
	"container/list"
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

	"github.com/logserve/logserve/internal/logrecord"
)

const (
	magic                              uint32 = 0x4c535647
	recordVersionV1                    uint16 = 1
	recordVersionV2                    uint16 = 2
	version                            uint16 = recordVersionV2
	legacyHeaderSize                          = 36
	headerSize                                = 40
	indexMagic                         uint32 = 0x4c534958
	indexVersion                       uint16 = 2
	legacyIndexVersion                 uint16 = 1
	indexFileHeaderSize                       = 40
	indexDictionaryHeaderSize                 = 8
	indexFixedEntrySize                       = 24
	legacyIndexRecordHeaderSize               = 36
	defaultSegmentSizeBytes            int64  = 64 << 20
	defaultSegmentReaderCacheSize             = 64
	defaultFsyncInterval                      = 100 * time.Millisecond
	defaultCompactionCopyLiveRatio            = 0.5
	defaultCompactionMaxBytesPerSecond        = 16 << 20
	rawReadBatchEntries                       = 1024
	retentionFileName                         = "retention.json"
)

var errCorruptRecord = errors.New("corrupt log record")
var errCorruptIndex = errors.New("corrupt index")
var indexCRCTable = crc32.MakeTable(crc32.Castagnoli)
var recordEncodeBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 64*1024)
		return &buf
	},
}

const maxPooledRecordEncodeBuffer = 4 << 20

type FsyncPolicy string

const (
	FsyncAlways   FsyncPolicy = "always"
	FsyncBatch    FsyncPolicy = "batch"
	FsyncInterval FsyncPolicy = "interval"
)

type Options struct {
	SegmentSizeBytes                 int64
	FsyncPolicy                      FsyncPolicy
	FsyncInterval                    time.Duration
	SegmentReaderCacheSize           int
	CompactionInterval               time.Duration
	CompactionCopyLiveRatioThreshold float64
	CompactionMaxBytesPerSecond      int64
	ChecksumType                     ChecksumType
	MmapRead                         bool
}

func DefaultOptions() Options {
	return Options{
		SegmentSizeBytes:                 defaultSegmentSizeBytes,
		FsyncPolicy:                      FsyncAlways,
		FsyncInterval:                    defaultFsyncInterval,
		SegmentReaderCacheSize:           defaultSegmentReaderCacheSize,
		CompactionCopyLiveRatioThreshold: defaultCompactionCopyLiveRatio,
		CompactionMaxBytesPerSecond:      defaultCompactionMaxBytesPerSecond,
		ChecksumType:                     ChecksumTypeCRC32C,
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
	if opts.SegmentReaderCacheSize < 0 {
		opts.SegmentReaderCacheSize = 0
	}
	if opts.CompactionInterval < 0 {
		return Options{}, errors.New("compaction interval cannot be negative")
	}
	if opts.CompactionCopyLiveRatioThreshold == 0 {
		opts.CompactionCopyLiveRatioThreshold = defaultCompactionCopyLiveRatio
	}
	if opts.CompactionCopyLiveRatioThreshold < 0 || opts.CompactionCopyLiveRatioThreshold > 1 {
		return Options{}, errors.New("compaction copy live-ratio threshold must be between 0 and 1")
	}
	if opts.CompactionMaxBytesPerSecond < 0 {
		return Options{}, errors.New("compaction max bytes per second cannot be negative")
	}
	if opts.CompactionMaxBytesPerSecond == 0 {
		opts.CompactionMaxBytesPerSecond = defaultCompactionMaxBytesPerSecond
	}
	if opts.ChecksumType == 0 {
		opts.ChecksumType = ChecksumTypeCRC32C
	}
	if err := validateChecksumType(opts.ChecksumType); err != nil {
		return Options{}, err
	}
	opts.MmapRead = opts.MmapRead || mmapReadFromEnv()
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
	streams            map[string]*streamState
	idempotency        map[string]Record
	segmentReaders     map[uint64]*segmentReader
	segmentReaderLRU   *list.List
	lastSync           time.Time
	compactorCancel    func()
	compactorDone      chan struct{}
}

type streamState struct {
	streamID   string
	nextSeq    uint64
	trimBefore uint64
	entries    []streamIndexEntry
}

type streamIndexEntry struct {
	Seq       uint64
	SegmentID uint64
	Offset    uint64
	Length    uint32
}

type recoveredIndexEntry struct {
	streamID string
	entry    streamIndexEntry
}

type segmentReader struct {
	segmentID uint64
	reader    SegmentReader
	refs      int
	element   *list.Element
	cached    bool
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
	if err := reconcileCompactionManifestBeforeRecover(dir); err != nil {
		return nil, err
	}

	s := &Store{
		dir:              dir,
		options:          normalized,
		streams:          make(map[string]*streamState),
		idempotency:      make(map[string]Record),
		segmentReaders:   make(map[uint64]*segmentReader),
		segmentReaderLRU: list.New(),
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
	s.startBackgroundCompactor()
	return s, nil
}

func (s *Store) Close() error {
	s.stopBackgroundCompactor()

	s.mu.Lock()
	defer s.mu.Unlock()

	return errors.Join(s.closeActiveFilesLocked(true), s.closeSegmentReadersLocked())
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

	state := s.streamStateLocked(req.StreamID)
	seq := state.nextSeq
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

	encoded, crc, err := encodeRecordPooled(rec, s.options.ChecksumType)
	if err != nil {
		return Record{}, false, err
	}
	defer putRecordEncodeBuffer(encoded)
	rec.CRC32 = crc
	rec.ChecksumType = s.options.ChecksumType
	if err := s.ensureWritableSegmentLocked(int64(len(encoded))); err != nil {
		return Record{}, false, err
	}
	if len(encoded) > int(^uint32(0)) {
		return Record{}, false, errors.New("record too large for index")
	}

	offset := s.activeSegmentBytes
	if _, err := s.logFile.Write(encoded); err != nil {
		return Record{}, false, err
	}
	entry := streamIndexEntry{
		Seq:       rec.Seq,
		SegmentID: s.activeSegmentID,
		Offset:    uint64(offset),
		Length:    uint32(len(encoded)),
	}
	if err := s.syncForPolicyLocked(); err != nil {
		return Record{}, false, err
	}

	s.activeSegmentBytes += int64(len(encoded))
	state.entries = append(state.entries, entry)
	state.nextSeq = seq + 1
	if rec.IdempotencyKey != "" {
		s.idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = idempotencyRecord(rec)
	}
	return rec, false, nil
}

func (s *Store) Read(streamID string, fromSeq uint64, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	out := make([]Record, 0, limit)
	err := s.ReadEach(streamID, fromSeq, limit, func(rec Record) error {
		out = append(out, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) ReadEach(streamID string, fromSeq uint64, limit int, emit func(Record) error) error {
	if streamID == "" {
		return errors.New("stream_id is required")
	}
	if emit == nil {
		return errors.New("emit is required")
	}
	if fromSeq == 0 {
		fromSeq = 1
	}

	s.mu.Lock()
	var selected []streamIndexEntry
	if state := s.streams[streamID]; state != nil {
		if state.trimBefore > fromSeq {
			fromSeq = state.trimBefore
		}
		entries := state.entries
		start := sort.Search(len(entries), func(i int) bool {
			return entries[i].Seq >= fromSeq
		})
		end := len(entries)
		if limit > 0 {
			end = min(start+limit, len(entries))
		}
		if start < end {
			selected = append([]streamIndexEntry(nil), entries[start:end]...)
		}
	}
	s.mu.Unlock()

	var current *segmentReader
	defer func() {
		if current != nil {
			s.releaseSegmentReader(current)
		}
	}()
	for _, entry := range selected {
		if current == nil || current.segmentID != entry.SegmentID {
			if current != nil {
				s.releaseSegmentReader(current)
			}
			reader, err := s.acquireSegmentReader(entry.SegmentID)
			if err != nil {
				current = nil
				return err
			}
			current = reader
		}
		rec, err := readIndexedRecordFromReader(current, streamID, entry)
		if err != nil {
			return err
		}
		if err := emit(rec); err != nil {
			return err
		}
	}
	return nil
}

// ReadRawEach emits records using scratch-backed payload slices. The emitted payload is valid until emit returns.
func (s *Store) ReadRawEach(streamID string, fromSeq uint64, limit int, emit func(logrecord.RawRecord) error) error {
	if streamID == "" {
		return errors.New("stream_id is required")
	}
	if emit == nil {
		return errors.New("emit is required")
	}
	if fromSeq == 0 {
		fromSeq = 1
	}

	nextSeq := fromSeq
	remaining := limit
	snapshotNextSeq := uint64(0)
	snapshotInitialized := false
	var scratch rawRecordReadScratch
	var current *segmentReader
	defer func() {
		if current != nil {
			s.releaseSegmentReader(current)
		}
	}()

	for {
		s.mu.Lock()
		state := s.streams[streamID]
		if state == nil || state.nextSeq == 0 {
			s.mu.Unlock()
			return nil
		}
		if !snapshotInitialized {
			if state.trimBefore > nextSeq {
				nextSeq = state.trimBefore
			}
			snapshotNextSeq = state.nextSeq
			snapshotInitialized = true
		}
		if nextSeq >= snapshotNextSeq {
			s.mu.Unlock()
			return nil
		}

		entries := state.entries
		start := sort.Search(len(entries), func(i int) bool {
			return entries[i].Seq >= nextSeq
		})
		maxEnd := sort.Search(len(entries), func(i int) bool {
			return entries[i].Seq >= snapshotNextSeq
		})
		batchSize := rawReadBatchEntries
		if remaining > 0 && remaining < batchSize {
			batchSize = remaining
		}
		end := min(start+batchSize, maxEnd)
		var selected []streamIndexEntry
		if start < end {
			selected = append([]streamIndexEntry(nil), entries[start:end]...)
		}
		s.mu.Unlock()

		if len(selected) == 0 {
			return nil
		}
		for _, entry := range selected {
			if current == nil || current.segmentID != entry.SegmentID {
				if current != nil {
					s.releaseSegmentReader(current)
				}
				reader, err := s.acquireSegmentReader(entry.SegmentID)
				if err != nil {
					current = nil
					return err
				}
				current = reader
			}
			rec, err := readIndexedRawRecordFromReader(current, streamID, entry, &scratch)
			if err != nil {
				return err
			}
			if err := emit(rec); err != nil {
				return err
			}
			nextSeq = entry.Seq + 1
			if remaining > 0 {
				remaining--
				if remaining == 0 {
					return nil
				}
			}
		}
	}
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

	state := s.streams[streamID]
	if state == nil || state.nextSeq == 0 {
		return TrimStats{}, fmt.Errorf("stream %q not found", streamID)
	}
	nextSeq := state.nextSeq
	if nextSeq == 0 {
		nextSeq = 1
	}
	if beforeSeq > nextSeq {
		beforeSeq = nextSeq
	}
	if beforeSeq < 1 {
		beforeSeq = 1
	}
	if beforeSeq > state.trimBefore {
		state.trimBefore = beforeSeq
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
	for id, state := range s.streams {
		if len(state.entries) > 0 || state.trimBefore > 0 {
			streams[id] = true
		}
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

	out := make([]string, 0, len(s.streams))
	for streamID, state := range s.streams {
		if len(state.entries) == 0 {
			continue
		}
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

	loadedFromIndex, usedLegacyIndex, err := s.loadFromIndexFiles(segmentIDs)
	if err == nil && loadedFromIndex {
		if usedLegacyIndex {
			return s.rewriteIndex()
		}
		return nil
	}

	s.resetRecoveredState()
	for _, segmentID := range segmentIDs {
		size, err := s.recoverSegment(segmentID)
		if err != nil {
			return err
		}
		s.activeSegmentID = segmentID
		s.activeSegmentBytes = size
	}
	s.sortStreamEntries()
	return s.rewriteIndex()
}

func (s *Store) loadFromIndexFiles(segmentIDs []uint64) (bool, bool, error) {
	usedLegacyIndex := false
	bySegment := make(map[uint64][]recoveredIndexEntry, len(segmentIDs))

	for _, segmentID := range segmentIDs {
		path := segmentPath(s.dir, segmentID, ".index")
		items, legacy, err := readSegmentIndex(path, segmentID)
		if errors.Is(err, os.ErrNotExist) {
			return false, usedLegacyIndex, nil
		}
		if err != nil {
			return false, usedLegacyIndex, err
		}
		usedLegacyIndex = usedLegacyIndex || legacy
		bySegment[segmentID] = items
		for _, item := range items {
			state := s.streamStateLocked(item.streamID)
			state.entries = append(state.entries, item.entry)
			if state.nextSeq <= item.entry.Seq {
				state.nextSeq = item.entry.Seq + 1
			}
		}
	}

	for _, segmentID := range segmentIDs {
		if err := validateIndexedSegmentCoverage(s.dir, segmentID, bySegment[segmentID]); err != nil {
			return false, usedLegacyIndex, err
		}
	}
	s.sortStreamEntries()
	if err := s.hydrateIdempotencyFromIndex(bySegment, segmentIDs); err != nil {
		return false, usedLegacyIndex, err
	}

	lastSegmentID := segmentIDs[len(segmentIDs)-1]
	info, err := os.Stat(segmentPath(s.dir, lastSegmentID, ".log"))
	if err != nil {
		return false, usedLegacyIndex, err
	}
	s.activeSegmentID = lastSegmentID
	s.activeSegmentBytes = info.Size()
	return true, usedLegacyIndex, nil
}

func (s *Store) resetRecoveredState() {
	s.streams = make(map[string]*streamState)
	s.idempotency = make(map[string]Record)
}

func (s *Store) streamStateLocked(streamID string) *streamState {
	state := s.streams[streamID]
	if state == nil {
		state = &streamState{streamID: streamID}
		s.streams[streamID] = state
	}
	return state
}

func (s *Store) sortStreamEntries() {
	for _, state := range s.streams {
		sort.Slice(state.entries, func(i, j int) bool {
			return state.entries[i].Seq < state.entries[j].Seq
		})
	}
}

func validateIndexedSegmentCoverage(dir string, segmentID uint64, items []recoveredIndexEntry) error {
	info, err := os.Stat(segmentPath(dir, segmentID, ".log"))
	if err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].entry.Offset < items[j].entry.Offset
	})
	var expectedOffset uint64
	for _, item := range items {
		if item.entry.Offset != expectedOffset {
			return errCorruptIndex
		}
		expectedOffset += uint64(item.entry.Length)
	}
	if expectedOffset != uint64(info.Size()) {
		return errCorruptIndex
	}
	return nil
}

func (s *Store) hydrateIdempotencyFromIndex(bySegment map[uint64][]recoveredIndexEntry, segmentIDs []uint64) error {
	for _, segmentID := range segmentIDs {
		items := bySegment[segmentID]
		sort.Slice(items, func(i, j int) bool {
			return items[i].entry.Offset < items[j].entry.Offset
		})
		file, err := os.Open(segmentPath(s.dir, segmentID, ".log"))
		if err != nil {
			return err
		}
		for _, item := range items {
			rec, err := readIndexedRecordFromFile(file, item.streamID, item.entry)
			if err != nil {
				_ = file.Close()
				return err
			}
			if rec.IdempotencyKey != "" {
				s.idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = idempotencyRecord(rec)
			}
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func readSegmentIndex(path string, segmentID uint64) ([]recoveredIndexEntry, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() == 0 {
		return nil, false, nil
	}

	reader := bufio.NewReader(file)
	probe, err := reader.Peek(4)
	if err != nil {
		return nil, false, errCorruptIndex
	}
	if binary.BigEndian.Uint32(probe) == indexMagic {
		versionProbe, err := reader.Peek(6)
		if err != nil {
			return nil, false, errCorruptIndex
		}
		switch binary.BigEndian.Uint16(versionProbe[4:6]) {
		case indexVersion:
			items, err := readDictionarySegmentIndex(reader, segmentID)
			return items, false, err
		case legacyIndexVersion:
			items, err := readLegacyBinarySegmentIndex(reader, segmentID)
			return items, true, err
		default:
			return nil, false, errCorruptIndex
		}
	}
	items, err := readJSONSegmentIndex(reader, segmentID)
	return items, true, err
}

func readDictionarySegmentIndex(r io.Reader, segmentID uint64) ([]recoveredIndexEntry, error) {
	header := make([]byte, indexFileHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, errCorruptIndex
	}
	if binary.BigEndian.Uint32(header[0:4]) != indexMagic {
		return nil, errCorruptIndex
	}
	if binary.BigEndian.Uint16(header[4:6]) != indexVersion {
		return nil, errCorruptIndex
	}
	if binary.BigEndian.Uint16(header[6:8]) != indexFileHeaderSize {
		return nil, errCorruptIndex
	}
	if binary.BigEndian.Uint64(header[8:16]) != segmentID {
		return nil, errCorruptIndex
	}
	streamCount := binary.BigEndian.Uint32(header[16:20])
	entryCount := binary.BigEndian.Uint64(header[20:28])
	dictionaryBytes := binary.BigEndian.Uint64(header[28:36])
	actualCRC := binary.BigEndian.Uint32(header[36:40])

	payload, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if dictionaryBytes > uint64(len(payload)) {
		return nil, errCorruptIndex
	}
	if uint64(streamCount)*uint64(indexDictionaryHeaderSize) > dictionaryBytes {
		return nil, errCorruptIndex
	}
	dictLen := int(dictionaryBytes)
	entryBytes := len(payload) - dictLen
	if entryBytes%indexFixedEntrySize != 0 || uint64(entryBytes/indexFixedEntrySize) != entryCount {
		return nil, errCorruptIndex
	}
	expectedCRC := crc32.Checksum(header[:36], indexCRCTable)
	expectedCRC = crc32.Update(expectedCRC, indexCRCTable, payload)
	if actualCRC != expectedCRC {
		return nil, errCorruptIndex
	}

	dictionary := make([]string, int(streamCount))
	dictPayload := payload[:dictLen]
	pos := 0
	for i := uint32(0); i < streamCount; i++ {
		if len(dictPayload)-pos < indexDictionaryHeaderSize {
			return nil, errCorruptIndex
		}
		streamIDID := binary.BigEndian.Uint32(dictPayload[pos : pos+4])
		streamLen := int(binary.BigEndian.Uint16(dictPayload[pos+4 : pos+6]))
		pos += indexDictionaryHeaderSize
		if streamIDID >= streamCount || streamLen == 0 || len(dictPayload)-pos < streamLen {
			return nil, errCorruptIndex
		}
		if dictionary[streamIDID] != "" {
			return nil, errCorruptIndex
		}
		dictionary[streamIDID] = string(dictPayload[pos : pos+streamLen])
		pos += streamLen
	}
	if pos != len(dictPayload) {
		return nil, errCorruptIndex
	}
	for _, streamID := range dictionary {
		if streamID == "" {
			return nil, errCorruptIndex
		}
	}

	items := make([]recoveredIndexEntry, 0, int(entryCount))
	entriesPayload := payload[dictLen:]
	for pos := 0; pos < len(entriesPayload); pos += indexFixedEntrySize {
		streamIDID := binary.BigEndian.Uint32(entriesPayload[pos : pos+4])
		seq := binary.BigEndian.Uint64(entriesPayload[pos+4 : pos+12])
		offset := binary.BigEndian.Uint64(entriesPayload[pos+12 : pos+20])
		length := binary.BigEndian.Uint32(entriesPayload[pos+20 : pos+24])
		if streamIDID >= streamCount || seq == 0 || length == 0 {
			return nil, errCorruptIndex
		}
		items = append(items, recoveredIndexEntry{
			streamID: dictionary[streamIDID],
			entry: streamIndexEntry{
				Seq:       seq,
				SegmentID: segmentID,
				Offset:    offset,
				Length:    length,
			},
		})
	}
	return items, nil
}

func readLegacyBinarySegmentIndex(r io.Reader, segmentID uint64) ([]recoveredIndexEntry, error) {
	items := make([]recoveredIndexEntry, 0)
	for {
		header := make([]byte, legacyIndexRecordHeaderSize)
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) {
				return items, nil
			}
			return nil, errCorruptIndex
		}
		if binary.BigEndian.Uint32(header[0:4]) != indexMagic {
			return nil, errCorruptIndex
		}
		if binary.BigEndian.Uint16(header[4:6]) != legacyIndexVersion {
			return nil, errCorruptIndex
		}
		streamLen := int(binary.BigEndian.Uint16(header[6:8]))
		seq := binary.BigEndian.Uint64(header[8:16])
		recordSegmentID := binary.BigEndian.Uint64(header[16:24])
		offset := binary.BigEndian.Uint64(header[24:32])
		length := binary.BigEndian.Uint32(header[32:36])
		if recordSegmentID != segmentID || streamLen == 0 || length == 0 {
			return nil, errCorruptIndex
		}
		streamAndCRC := make([]byte, streamLen+4)
		if _, err := io.ReadFull(r, streamAndCRC); err != nil {
			return nil, errCorruptIndex
		}
		actualCRC := binary.BigEndian.Uint32(streamAndCRC[streamLen:])
		expectedCRC := crc32.Checksum(header, indexCRCTable)
		expectedCRC = crc32.Update(expectedCRC, indexCRCTable, streamAndCRC[:streamLen])
		if actualCRC != expectedCRC {
			return nil, errCorruptIndex
		}
		items = append(items, recoveredIndexEntry{
			streamID: string(streamAndCRC[:streamLen]),
			entry: streamIndexEntry{
				Seq:       seq,
				SegmentID: recordSegmentID,
				Offset:    offset,
				Length:    length,
			},
		})
	}
}

func readJSONSegmentIndex(r io.Reader, segmentID uint64) ([]recoveredIndexEntry, error) {
	decoder := json.NewDecoder(r)
	items := make([]recoveredIndexEntry, 0)
	for {
		var raw struct {
			StreamID  string `json:"stream_id"`
			Seq       uint64 `json:"seq"`
			SegmentID uint64 `json:"segment_id"`
			Offset    int64  `json:"offset"`
			Length    int64  `json:"length"`
		}
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return items, nil
			}
			return nil, err
		}
		if raw.StreamID == "" || raw.Seq == 0 || raw.SegmentID != segmentID || raw.Offset < 0 || raw.Length <= 0 || raw.Length > int64(^uint32(0)) {
			return nil, errCorruptIndex
		}
		items = append(items, recoveredIndexEntry{
			streamID: raw.StreamID,
			entry: streamIndexEntry{
				Seq:       raw.Seq,
				SegmentID: raw.SegmentID,
				Offset:    uint64(raw.Offset),
				Length:    uint32(raw.Length),
			},
		})
	}
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
		state := s.streamStateLocked(streamID)
		state.trimBefore = meta.TrimmedBeforeSeq
		if state.nextSeq < meta.TrimmedBeforeSeq {
			state.nextSeq = meta.TrimmedBeforeSeq
		}
	}
	return nil
}

func (s *Store) persistRetentionLocked() error {
	file := retentionFile{Streams: make(map[string]retentionStream, len(s.streams))}
	now := time.Now().UnixMilli()
	for streamID, state := range s.streams {
		if state.trimBefore == 0 {
			continue
		}
		file.Streams[streamID] = retentionStream{
			TrimmedBeforeSeq: state.trimBefore,
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
	state := s.streams[streamID]
	trimmedBefore := uint64(0)
	nextSeq := uint64(1)
	if state != nil {
		trimmedBefore = state.trimBefore
		nextSeq = state.nextSeq
		if nextSeq == 0 {
			nextSeq = 1
		}
	}
	stats := TrimStats{
		StreamID:         streamID,
		NextSeq:          nextSeq,
		TrimmedBeforeSeq: trimmedBefore,
	}
	if state != nil {
		for _, entry := range state.entries {
			if trimmedBefore > 0 && entry.Seq < trimmedBefore {
				stats.CompactableRecords++
				stats.CompactableBytes += uint64(entry.Length)
				continue
			}
			if stats.FirstSeq == 0 || entry.Seq < stats.FirstSeq {
				stats.FirstSeq = entry.Seq
			}
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
		length := nextOffset - offset
		if length <= 0 || length > int64(^uint32(0)) {
			return 0, errors.New("record too large for index")
		}
		entry := streamIndexEntry{
			Seq:       rec.Seq,
			SegmentID: segmentID,
			Offset:    uint64(offset),
			Length:    uint32(length),
		}
		state := s.streamStateLocked(rec.StreamID)
		state.entries = append(state.entries, entry)
		if state.nextSeq <= rec.Seq {
			state.nextSeq = rec.Seq + 1
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

	for _, segmentID := range segmentIDs {
		file, err := os.OpenFile(segmentPath(s.dir, segmentID, ".index"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if err := writeSegmentIndex(file, segmentID, s.segmentIndexEntriesLocked(segmentID)); err != nil {
			_ = file.Close()
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) segmentIndexEntriesLocked(segmentID uint64) []recoveredIndexEntry {
	entries := make([]recoveredIndexEntry, 0)
	for streamID, state := range s.streams {
		for _, entry := range state.entries {
			if entry.SegmentID != segmentID {
				continue
			}
			entries = append(entries, recoveredIndexEntry{
				streamID: streamID,
				entry:    entry,
			})
		}
	}
	return entries
}

func (s *Store) flushActiveIndexLocked() error {
	if s.indexFile == nil || s.activeSegmentID == 0 {
		return nil
	}
	if err := s.indexFile.Truncate(0); err != nil {
		return err
	}
	if _, err := s.indexFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeSegmentIndex(s.indexFile, s.activeSegmentID, s.segmentIndexEntriesLocked(s.activeSegmentID))
}

func writeSegmentIndex(w io.Writer, segmentID uint64, entries []recoveredIndexEntry) error {
	entries = append([]recoveredIndexEntry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].entry.Offset < entries[j].entry.Offset
	})

	streamSet := make(map[string]struct{})
	for _, item := range entries {
		if item.streamID == "" {
			return errors.New("stream_id is required")
		}
		if len(item.streamID) > 0xffff {
			return errors.New("stream_id too large")
		}
		if item.entry.SegmentID != segmentID || item.entry.Seq == 0 || item.entry.Length == 0 {
			return errCorruptIndex
		}
		streamSet[item.streamID] = struct{}{}
	}
	if uint64(len(streamSet)) > uint64(^uint32(0)) {
		return errors.New("too many streams in segment index")
	}

	streamIDs := make([]string, 0, len(streamSet))
	for streamID := range streamSet {
		streamIDs = append(streamIDs, streamID)
	}
	sort.Strings(streamIDs)
	streamIDsByName := make(map[string]uint32, len(streamIDs))

	var dictionary bytes.Buffer
	for i, streamID := range streamIDs {
		streamIDID := uint32(i)
		streamBytes := []byte(streamID)
		streamIDsByName[streamID] = streamIDID
		header := make([]byte, indexDictionaryHeaderSize)
		binary.BigEndian.PutUint32(header[0:4], streamIDID)
		binary.BigEndian.PutUint16(header[4:6], uint16(len(streamBytes)))
		dictionary.Write(header)
		dictionary.Write(streamBytes)
	}

	var fixedEntries bytes.Buffer
	for _, item := range entries {
		entry := make([]byte, indexFixedEntrySize)
		binary.BigEndian.PutUint32(entry[0:4], streamIDsByName[item.streamID])
		binary.BigEndian.PutUint64(entry[4:12], item.entry.Seq)
		binary.BigEndian.PutUint64(entry[12:20], item.entry.Offset)
		binary.BigEndian.PutUint32(entry[20:24], item.entry.Length)
		fixedEntries.Write(entry)
	}

	payload := append(dictionary.Bytes(), fixedEntries.Bytes()...)
	header := make([]byte, indexFileHeaderSize)
	binary.BigEndian.PutUint32(header[0:4], indexMagic)
	binary.BigEndian.PutUint16(header[4:6], indexVersion)
	binary.BigEndian.PutUint16(header[6:8], indexFileHeaderSize)
	binary.BigEndian.PutUint64(header[8:16], segmentID)
	binary.BigEndian.PutUint32(header[16:20], uint32(len(streamIDs)))
	binary.BigEndian.PutUint64(header[20:28], uint64(len(entries)))
	binary.BigEndian.PutUint64(header[28:36], uint64(dictionary.Len()))
	crc := crc32.Checksum(header[:36], indexCRCTable)
	crc = crc32.Update(crc, indexCRCTable, payload)
	binary.BigEndian.PutUint32(header[36:40], crc)

	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func (s *Store) acquireSegmentReader(segmentID uint64) (*segmentReader, error) {
	s.mu.Lock()
	activeSegmentID := s.activeSegmentID
	mmapRead := s.options.MmapRead
	cacheSize := s.options.SegmentReaderCacheSize
	dir := s.dir
	if cacheSize > 0 {
		if reader := s.segmentReaders[segmentID]; reader != nil {
			reader.refs++
			if reader.element != nil {
				s.segmentReaderLRU.MoveToBack(reader.element)
			}
			s.mu.Unlock()
			return reader, nil
		}
	}
	s.mu.Unlock()

	sealed := activeSegmentID != 0 && segmentID < activeSegmentID
	segReader, err := openSegmentReader(dir, segmentID, sealed, mmapRead)
	if err != nil {
		return nil, err
	}
	reader := &segmentReader{segmentID: segmentID, reader: segReader, refs: 1}

	if cacheSize <= 0 {
		return reader, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.segmentReaders[segmentID]; existing != nil {
		_ = segReader.Close()
		existing.refs++
		if existing.element != nil {
			s.segmentReaderLRU.MoveToBack(existing.element)
		}
		return existing, nil
	}
	reader.cached = true
	reader.element = s.segmentReaderLRU.PushBack(reader)
	s.segmentReaders[segmentID] = reader
	s.evictSegmentReadersLocked()
	return reader, nil
}

func (s *Store) releaseSegmentReader(reader *segmentReader) {
	if reader == nil {
		return
	}
	if !reader.cached {
		_ = reader.reader.Close()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if reader.refs > 0 {
		reader.refs--
	}
	if reader.refs == 0 && reader.element != nil {
		s.segmentReaderLRU.MoveToBack(reader.element)
	}
	s.evictSegmentReadersLocked()
}

func (s *Store) evictSegmentReadersLocked() {
	limit := s.options.SegmentReaderCacheSize
	if limit <= 0 {
		return
	}
	for len(s.segmentReaders) > limit {
		var evicted bool
		for element := s.segmentReaderLRU.Front(); element != nil; {
			next := element.Next()
			reader := element.Value.(*segmentReader)
			if reader.refs == 0 {
				s.segmentReaderLRU.Remove(element)
				delete(s.segmentReaders, reader.segmentID)
				reader.cached = false
				reader.element = nil
				_ = reader.reader.Close()
				evicted = true
				break
			}
			element = next
		}
		if !evicted {
			return
		}
	}
}

func (s *Store) closeSegmentReadersLocked() error {
	var err error
	for segmentID, reader := range s.segmentReaders {
		err = errors.Join(err, reader.reader.Close())
		delete(s.segmentReaders, segmentID)
		reader.cached = false
		reader.element = nil
	}
	s.segmentReaderLRU.Init()
	return err
}

func (s *Store) cachedSegmentReaderCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.segmentReaders)
}
func readIndexedRawRecordFromReader(reader *segmentReader, streamID string, entry streamIndexEntry, scratch *rawRecordReadScratch) (logrecord.RawRecord, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if entry.Offset > maxInt64 {
		return logrecord.RawRecord{}, errCorruptIndex
	}
	offset := int64(entry.Offset)
	io := &segmentIO{reader: reader.reader}
	rec, _, nextOffset, err := readRawRecordAtWithScratch(io, offset, scratch)
	if err != nil {
		return logrecord.RawRecord{}, err
	}
	if rec.StreamID != streamID || rec.Seq != entry.Seq || nextOffset-offset != int64(entry.Length) {
		return logrecord.RawRecord{}, errCorruptRecord
	}
	return rec, nil
}

func readIndexedRecordFromReader(reader *segmentReader, streamID string, entry streamIndexEntry) (Record, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if entry.Offset > maxInt64 {
		return Record{}, errCorruptIndex
	}
	offset := int64(entry.Offset)
	io := &segmentIO{reader: reader.reader}
	rec, nextOffset, err := readRecordAtIO(io, offset)
	if err != nil {
		return Record{}, err
	}
	if rec.StreamID != streamID || rec.Seq != entry.Seq || nextOffset-offset != int64(entry.Length) {
		return Record{}, errCorruptRecord
	}
	return cloneRecord(rec), nil
}

func readIndexedRecordFromFile(file *os.File, streamID string, entry streamIndexEntry) (Record, error) {
	return readIndexedRecordFromReader(&segmentReader{reader: &readAtSegmentReader{file: file}}, streamID, entry)
}

func readIndexedRawRecordFromFile(file *os.File, streamID string, entry streamIndexEntry, scratch *rawRecordReadScratch) (logrecord.RawRecord, error) {
	return readIndexedRawRecordFromReader(&segmentReader{reader: &readAtSegmentReader{file: file}}, streamID, entry, scratch)
}

func mmapReadFromEnv() bool {
	v := strings.TrimSpace(os.Getenv("LOGSERVE_LOG_MMAP_READ"))
	return v == "1" || strings.EqualFold(v, "true")
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
	indexFile, err := os.OpenFile(segmentPath(s.dir, s.activeSegmentID, ".index"), os.O_CREATE|os.O_RDWR, 0o644)
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
		err = errors.Join(err, s.flushActiveIndexLocked())
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

func encodeRecord(rec Record, typ ChecksumType) ([]byte, uint32, error) {
	return encodeRecordWithBuffer(rec, typ, nil)
}

func encodeRecordPooled(rec Record, typ ChecksumType) ([]byte, uint32, error) {
	if typ == 0 {
		typ = ChecksumTypeCRC32C
	}
	size, err := encodedRecordSize(rec, typ)
	if err != nil {
		return nil, 0, err
	}
	buf := getRecordEncodeBuffer(size)
	encoded, crc, err := encodeRecordWithBuffer(rec, typ, buf)
	if err != nil {
		putRecordEncodeBuffer(buf)
		return nil, 0, err
	}
	return encoded, crc, nil
}
func encodeRecordWithBuffer(rec Record, typ ChecksumType, buf []byte) ([]byte, uint32, error) {
	if typ == 0 {
		typ = ChecksumTypeCRC32C
	}
	size, err := encodedRecordSize(rec, typ)
	if err != nil {
		return nil, 0, err
	}
	if len(buf) < size {
		buf = make([]byte, size)
	} else {
		buf = buf[:size]
	}

	streamLen := len(rec.StreamID)
	eventLen := len(rec.EventType)
	keyLen := len(rec.IdempotencyKey)

	binary.BigEndian.PutUint32(buf[0:4], magic)
	binary.BigEndian.PutUint16(buf[4:6], version)
	binary.BigEndian.PutUint16(buf[6:8], uint16(streamLen))
	binary.BigEndian.PutUint16(buf[8:10], uint16(eventLen))
	binary.BigEndian.PutUint16(buf[10:12], uint16(keyLen))
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(rec.Payload)))
	binary.BigEndian.PutUint64(buf[16:24], rec.Seq)
	binary.BigEndian.PutUint64(buf[24:32], uint64(rec.TimestampMs))
	binary.BigEndian.PutUint16(buf[36:38], uint16(typ))

	pos := headerSize
	pos += copy(buf[pos:], rec.StreamID)
	pos += copy(buf[pos:], rec.EventType)
	pos += copy(buf[pos:], rec.IdempotencyKey)
	copy(buf[pos:], rec.Payload)

	crc, err := checksum(buf[headerSize:], typ)
	if err != nil {
		return nil, 0, err
	}
	binary.BigEndian.PutUint32(buf[32:36], crc)
	return buf, crc, nil
}

func encodedRecordSize(rec Record, typ ChecksumType) (int, error) {
	if len(rec.StreamID) > 0xffff {
		return 0, errors.New("stream_id too large")
	}
	if len(rec.EventType) > 0xffff {
		return 0, errors.New("event_type too large")
	}
	if len(rec.IdempotencyKey) > 0xffff {
		return 0, errors.New("idempotency_key too large")
	}
	if typ == 0 {
		typ = ChecksumTypeCRC32C
	}
	if err := validateChecksumType(typ); err != nil {
		return 0, err
	}
	bodyLen := len(rec.StreamID) + len(rec.EventType) + len(rec.IdempotencyKey) + len(rec.Payload)
	return headerSize + bodyLen, nil
}

func getRecordEncodeBuffer(size int) []byte {
	if size > maxPooledRecordEncodeBuffer {
		return make([]byte, size)
	}
	ptr := recordEncodeBufferPool.Get().(*[]byte)
	buf := *ptr
	if cap(buf) < size {
		return make([]byte, size)
	}
	return buf[:size]
}

func putRecordEncodeBuffer(buf []byte) {
	if cap(buf) == 0 || cap(buf) > maxPooledRecordEncodeBuffer {
		return
	}
	buf = buf[:0]
	recordEncodeBufferPool.Put(&buf)
}

type rawRecordReadScratch struct {
	header [headerSize]byte
	body   []byte
}

func readRecordAt(file *os.File, offset int64) (Record, int64, error) {
	return readRecordAtIO(&segmentIO{reader: &readAtSegmentReader{file: file}}, offset)
}

func readRecordAtIO(io *segmentIO, offset int64) (Record, int64, error) {
	var scratch rawRecordReadScratch
	raw, checksumType, nextOffset, err := readRawRecordAtWithScratch(io, offset, &scratch)
	if err != nil {
		return Record{}, offset, err
	}
	return Record{
		StreamID:       raw.StreamID,
		Seq:            raw.Seq,
		EventType:      raw.EventType,
		IdempotencyKey: raw.IdempotencyKey,
		Payload:        append([]byte(nil), raw.Payload...),
		TimestampMs:    raw.TimestampMs,
		CRC32:          raw.CRC32,
		ChecksumType:   checksumType,
	}, nextOffset, nil
}

func readRawRecordAt(file *os.File, offset int64) (logrecord.RawRecord, ChecksumType, int64, error) {
	return readRawRecordAtWithScratch(&segmentIO{reader: &readAtSegmentReader{file: file}}, offset, nil)
}

func readRawRecordAtWithScratch(src *segmentIO, offset int64, scratch *rawRecordReadScratch) (logrecord.RawRecord, ChecksumType, int64, error) {
	if scratch == nil {
		scratch = &rawRecordReadScratch{}
	}
	if header, ok := src.slice(offset, legacyHeaderSize); ok {
		return readRawRecordFromMmap(src, offset, header, scratch)
	}
	return readRawRecordFromReadAt(src, offset, scratch)
}

func readRawRecordFromMmap(src *segmentIO, offset int64, baseHeader []byte, scratch *rawRecordReadScratch) (logrecord.RawRecord, ChecksumType, int64, error) {
	if len(baseHeader) != legacyHeaderSize {
		return logrecord.RawRecord{}, 0, offset, errCorruptRecord
	}
	if binary.BigEndian.Uint32(baseHeader[0:4]) != magic {
		return logrecord.RawRecord{}, 0, offset, errCorruptRecord
	}
	recordVersion := binary.BigEndian.Uint16(baseHeader[4:6])
	headerLen := legacyHeaderSize
	checksumType := ChecksumTypeIEEE
	switch recordVersion {
	case recordVersionV1:
	case recordVersionV2:
		extension, ok := src.slice(offset+legacyHeaderSize, headerSize-legacyHeaderSize)
		if !ok || len(extension) != headerSize-legacyHeaderSize {
			return logrecord.RawRecord{}, 0, offset, errCorruptRecord
		}
		copy(scratch.header[legacyHeaderSize:headerSize], extension)
		checksumType = ChecksumType(binary.BigEndian.Uint16(extension[0:2]))
		if err := validateChecksumType(checksumType); err != nil {
			return logrecord.RawRecord{}, 0, offset, errCorruptRecord
		}
		headerLen = headerSize
	default:
		return logrecord.RawRecord{}, 0, offset, errCorruptRecord
	}
	copy(scratch.header[:legacyHeaderSize], baseHeader)

	streamLen := int(binary.BigEndian.Uint16(baseHeader[6:8]))
	eventLen := int(binary.BigEndian.Uint16(baseHeader[8:10]))
	keyLen := int(binary.BigEndian.Uint16(baseHeader[10:12]))
	payloadLen := int(binary.BigEndian.Uint32(baseHeader[12:16]))
	seq := binary.BigEndian.Uint64(baseHeader[16:24])
	timestampMs := int64(binary.BigEndian.Uint64(baseHeader[24:32]))
	expectedCRC := binary.BigEndian.Uint32(baseHeader[32:36])

	bodyLen := streamLen + eventLen + keyLen + payloadLen
	body, err := readBodyVerifiedIO(src, offset+int64(headerLen), bodyLen, checksumType, expectedCRC, scratch)
	if err != nil {
		return logrecord.RawRecord{}, 0, offset, err
	}

	pos := 0
	streamID := string(body[pos : pos+streamLen])
	pos += streamLen
	eventType := string(body[pos : pos+eventLen])
	pos += eventLen
	key := string(body[pos : pos+keyLen])
	pos += keyLen
	payload := body[pos : pos+payloadLen]

	return logrecord.RawRecord{
		StreamID:       streamID,
		Seq:            seq,
		EventType:      eventType,
		IdempotencyKey: key,
		Payload:        payload,
		TimestampMs:    timestampMs,
		CRC32:          expectedCRC,
	}, checksumType, offset + int64(headerLen+bodyLen), nil
}

func readRawRecordFromReadAt(src *segmentIO, offset int64, scratch *rawRecordReadScratch) (logrecord.RawRecord, ChecksumType, int64, error) {
	baseHeader := scratch.header[:legacyHeaderSize]
	n, err := src.ReadAt(baseHeader, offset)
	if errors.Is(err, io.EOF) && n == 0 {
		return logrecord.RawRecord{}, 0, offset, io.EOF
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return logrecord.RawRecord{}, 0, offset, err
	}
	if n != legacyHeaderSize {
		return logrecord.RawRecord{}, 0, offset, errCorruptRecord
	}

	if binary.BigEndian.Uint32(baseHeader[0:4]) != magic {
		return logrecord.RawRecord{}, 0, offset, errCorruptRecord
	}
	recordVersion := binary.BigEndian.Uint16(baseHeader[4:6])
	headerLen := legacyHeaderSize
	checksumType := ChecksumTypeIEEE
	switch recordVersion {
	case recordVersionV1:
	case recordVersionV2:
		extension := scratch.header[legacyHeaderSize:headerSize]
		n, err = src.ReadAt(extension, offset+legacyHeaderSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return logrecord.RawRecord{}, 0, offset, err
		}
		if n != len(extension) {
			return logrecord.RawRecord{}, 0, offset, errCorruptRecord
		}
		checksumType = ChecksumType(binary.BigEndian.Uint16(extension[0:2]))
		if err := validateChecksumType(checksumType); err != nil {
			return logrecord.RawRecord{}, 0, offset, errCorruptRecord
		}
		headerLen = headerSize
	default:
		return logrecord.RawRecord{}, 0, offset, errCorruptRecord
	}

	streamLen := int(binary.BigEndian.Uint16(baseHeader[6:8]))
	eventLen := int(binary.BigEndian.Uint16(baseHeader[8:10]))
	keyLen := int(binary.BigEndian.Uint16(baseHeader[10:12]))
	payloadLen := int(binary.BigEndian.Uint32(baseHeader[12:16]))
	seq := binary.BigEndian.Uint64(baseHeader[16:24])
	timestampMs := int64(binary.BigEndian.Uint64(baseHeader[24:32]))
	expectedCRC := binary.BigEndian.Uint32(baseHeader[32:36])

	bodyLen := streamLen + eventLen + keyLen + payloadLen
	body, err := readBodyVerifiedIO(src, offset+int64(headerLen), bodyLen, checksumType, expectedCRC, scratch)
	if err != nil {
		return logrecord.RawRecord{}, 0, offset, err
	}

	pos := 0
	streamID := string(body[pos : pos+streamLen])
	pos += streamLen
	eventType := string(body[pos : pos+eventLen])
	pos += eventLen
	key := string(body[pos : pos+keyLen])
	pos += keyLen
	payload := body[pos : pos+payloadLen]

	return logrecord.RawRecord{
		StreamID:       streamID,
		Seq:            seq,
		EventType:      eventType,
		IdempotencyKey: key,
		Payload:        payload,
		TimestampMs:    timestampMs,
		CRC32:          expectedCRC,
	}, checksumType, offset + int64(headerLen+bodyLen), nil
}
func readBodyVerifiedIO(src *segmentIO, offset int64, bodyLen int, checksumType ChecksumType, expectedCRC uint32, scratch *rawRecordReadScratch) ([]byte, error) {
	if body, ok := src.slice(offset, bodyLen); ok {
		if !verifyChecksum(body, checksumType, expectedCRC) {
			return nil, errCorruptRecord
		}
		return body, nil
	}

	if cap(scratch.body) < bodyLen {
		scratch.body = make([]byte, bodyLen)
	}
	body := scratch.body[:bodyLen]

	useChunkedRead := bodyLen > checksumChunkSize &&
		(checksumType == ChecksumTypeIEEE || checksumType == ChecksumTypeCRC32C)
	if !useChunkedRead {
		n, err := src.ReadAt(body, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n != bodyLen {
			return nil, errCorruptRecord
		}
		if !verifyChecksum(body, checksumType, expectedCRC) {
			return nil, errCorruptRecord
		}
		return body, nil
	}

	acc := newChecksumAccumulator(checksumType)
	for start := 0; start < bodyLen; start += checksumChunkSize {
		end := start + checksumChunkSize
		if end > bodyLen {
			end = bodyLen
		}
		chunk := body[start:end]
		n, err := src.ReadAt(chunk, offset+int64(start))
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n != len(chunk) {
			return nil, errCorruptRecord
		}
		if err := acc.update(chunk); err != nil {
			return nil, errCorruptRecord
		}
	}
	actualCRC, err := acc.sum()
	if err != nil || actualCRC != expectedCRC {
		return nil, errCorruptRecord
	}
	return body, nil
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
