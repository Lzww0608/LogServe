package logstore

import (
	"errors"
	"hash/crc32"

	"github.com/zeebo/xxh3"
)

type ChecksumType uint16

const (
	ChecksumTypeIEEE ChecksumType = iota + 1
	ChecksumTypeCRC32C
	ChecksumTypeXXH3
	ChecksumTypeNone
)

var checksumCRC32CTable = crc32.MakeTable(crc32.Castagnoli)

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
	switch typ {
	case ChecksumTypeIEEE:
		return crc32.ChecksumIEEE(data), nil
	case ChecksumTypeCRC32C:
		return crc32.Checksum(data, checksumCRC32CTable), nil
	case ChecksumTypeXXH3:
		return uint32(xxh3.Hash(data)), nil
	case ChecksumTypeNone:
		return 0, nil
	default:
		return 0, errors.New("unsupported checksum type")
	}
}
