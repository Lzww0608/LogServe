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
