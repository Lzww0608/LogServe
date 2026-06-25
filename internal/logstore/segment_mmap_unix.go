//go:build linux || darwin

package logstore

import (
	"errors"
	"os"
	"syscall"
)

const maxMmapSegmentBytes = 1 << 30

type mmapMapping struct {
	data []byte
}

func mmapSupported() bool { return true }

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

func (m *mmapMapping) Close() error {
	if m == nil || len(m.data) == 0 {
		return nil
	}
	err := syscall.Munmap(m.data)
	m.data = nil
	return err
}
