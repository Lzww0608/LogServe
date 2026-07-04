//go:build linux || darwin

package logstore

import (
	"errors"
	"os"
	"syscall"
)

// maxMmapSegmentBytes keeps accidental huge mappings out of this lightweight
// local store; larger segments still work through the ReadAt fallback.
const maxMmapSegmentBytes = 1 << 30

// mmapMapping owns a syscall.Mmap byte slice until Close unmaps it.
type mmapMapping struct {
	data []byte
}

// mmapSupported reports that this build target has mmap read support.
func mmapSupported() bool { return true }

// mmapFile maps the full file for read-only random access. Empty files produce
// an empty mapping so callers can still fall back to ReadAt semantics.
func mmapFile(file *os.File) (*mmapMapping, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return &mmapMapping{}, nil
	}
	if size > maxMmapSegmentBytes {
		return nil, errors.New("segment exceeds mmap size limit")
	}
	data, err := syscall.Mmap(int(file.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &mmapMapping{data: data}, nil
}

// Close releases the mapped address range and clears the slice to prevent reuse
// after unmap.
func (m *mmapMapping) Close() error {
	if m == nil || len(m.data) == 0 {
		return nil
	}
	err := syscall.Munmap(m.data)
	m.data = nil
	return err
}
