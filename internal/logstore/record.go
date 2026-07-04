package logstore

// Record is the durable log entry returned by safe readers and append calls.
// Payload is owned by the caller; read paths clone it so external mutation does
// not corrupt store-owned buffers.
type Record struct {
	StreamID       string
	Seq            uint64
	EventType      string
	IdempotencyKey string
	Payload        []byte
	TimestampMs    int64
	CRC32          uint32
	ChecksumType   ChecksumType
}

// AppendRequest contains the user-supplied fields for appending a record. The
// store assigns sequence, timestamp, checksum, and segment location metadata.
type AppendRequest struct {
	StreamID       string
	EventType      string
	IdempotencyKey string
	Payload        []byte
}

// TrimStats summarizes logical retention for a stream. Compactable counts are
// derived from records below TrimmedBeforeSeq and become reclaimable only after
// segment compaction removes their bytes from disk.
type TrimStats struct {
	StreamID           string
	FirstSeq           uint64
	NextSeq            uint64
	TrimmedBeforeSeq   uint64
	CompactableRecords uint64
	CompactableBytes   uint64
}
