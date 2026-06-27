//go:build !linux && !darwin

package logstore

import "os"

type mmapMapping struct {
	data []byte
}

func mmapSupported() bool { return false }

func mmapFile(_ *os.File) (*mmapMapping, error) { return nil, errMmapUnsupported }

func (m *mmapMapping) Close() error { return nil }
