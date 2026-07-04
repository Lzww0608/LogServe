//go:build !linux && !darwin

package logstore

import "os"

// mmapMapping is the stub mapping shape used on platforms without mmap read
// support. Its data is always empty.
type mmapMapping struct {
	data []byte
}

// mmapSupported reports that this build target cannot mmap segment files.
func mmapSupported() bool { return false }

// mmapFile returns the shared unsupported error on non-Unix platforms.
func mmapFile(_ *os.File) (*mmapMapping, error) { return nil, errMmapUnsupported }

// Close is a no-op for the stub mapping.
func (m *mmapMapping) Close() error { return nil }
