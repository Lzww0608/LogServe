package logstore

import (
	"errors"
	"hash/crc32"

	kpcrc32 "github.com/klauspost/crc32"
	"github.com/zeebo/xxh3"
)

const checksumChunkSize = 64 << 10

type ChecksumType uint16

const (
	ChecksumTypeIEEE ChecksumType = iota + 1
	ChecksumTypeCRC32C
	ChecksumTypeXXH3
	ChecksumTypeNone
)

var (
	checksumCRC32CTable = kpcrc32.MakeTable(kpcrc32.Castagnoli)
	checksumIEEETable   = crc32.IEEETable
)

func (typ ChecksumType) String() string {
	switch typ {
	case ChecksumTypeIEEE:
		return "IEEE"
	case ChecksumTypeCRC32C:
		return "CRC32C"
	case ChecksumTypeXXH3:
		return "XXH3"
	case ChecksumTypeNone:
		return "None"
	default:
		return "Unknown"
	}
}

func validateChecksumType(typ ChecksumType) error {
	switch typ {
	case ChecksumTypeIEEE, ChecksumTypeCRC32C, ChecksumTypeXXH3, ChecksumTypeNone:
		return nil
	default:
		return errors.New("unsupported checksum type")
	}
}

func checksum(data []byte, typ ChecksumType) (uint32, error) {
	return checksumOnce(data, typ)
}

func checksumOnce(data []byte, typ ChecksumType) (uint32, error) {
	switch typ {
	case ChecksumTypeIEEE:
		return crc32.ChecksumIEEE(data), nil
	case ChecksumTypeCRC32C:
		return kpcrc32.Checksum(data, checksumCRC32CTable), nil
	case ChecksumTypeXXH3:
		return uint32(xxh3.Hash(data)), nil
	case ChecksumTypeNone:
		return 0, nil
	default:
		return 0, errors.New("unsupported checksum type")
	}
}

func checksumChunked(data []byte, typ ChecksumType) (uint32, error) {
	switch typ {
	case ChecksumTypeIEEE:
		crc := uint32(0)
		for start := 0; start < len(data); start += checksumChunkSize {
			end := start + checksumChunkSize
			if end > len(data) {
				end = len(data)
			}
			crc = crc32.Update(crc, checksumIEEETable, data[start:end])
		}
		return crc, nil
	case ChecksumTypeCRC32C:
		crc := uint32(0)
		for start := 0; start < len(data); start += checksumChunkSize {
			end := start + checksumChunkSize
			if end > len(data) {
				end = len(data)
			}
			crc = kpcrc32.Update(crc, checksumCRC32CTable, data[start:end])
		}
		return crc, nil
	case ChecksumTypeXXH3:
		return uint32(xxh3.Hash(data)), nil
	case ChecksumTypeNone:
		return 0, nil
	default:
		return 0, errors.New("unsupported checksum type")
	}
}

func verifyChecksum(data []byte, typ ChecksumType, expected uint32) bool {
	actual, err := checksum(data, typ)
	return err == nil && actual == expected
}

type checksumAccumulator struct {
	typ ChecksumType
	crc uint32
}

func newChecksumAccumulator(typ ChecksumType) checksumAccumulator {
	return checksumAccumulator{typ: typ}
}

func (a *checksumAccumulator) update(data []byte) error {
	switch a.typ {
	case ChecksumTypeIEEE:
		a.crc = crc32.Update(a.crc, checksumIEEETable, data)
	case ChecksumTypeCRC32C:
		a.crc = kpcrc32.Update(a.crc, checksumCRC32CTable, data)
	case ChecksumTypeXXH3, ChecksumTypeNone:
	default:
		return errors.New("unsupported checksum type")
	}
	return nil
}

func (a *checksumAccumulator) sum() (uint32, error) {
	switch a.typ {
	case ChecksumTypeIEEE, ChecksumTypeCRC32C:
		return a.crc, nil
	default:
		return 0, errors.New("unsupported checksum type")
	}
}
