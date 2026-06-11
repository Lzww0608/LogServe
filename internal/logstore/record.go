package logstore

type Record struct {
	StreamID       string
	Seq            uint64
	EventType      string
	IdempotencyKey string
	Payload        []byte
	TimestampMs    int64
	CRC32          uint32
}

type AppendRequest struct {
	StreamID       string
	EventType      string
	IdempotencyKey string
	Payload        []byte
}

type TrimStats struct {
	StreamID           string
	FirstSeq           uint64
	NextSeq            uint64
	TrimmedBeforeSeq   uint64
	CompactableRecords uint64
	CompactableBytes   uint64
}
