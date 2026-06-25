//go:build !linux && !darwin

package logstore

import "os"

func mmapSupported() bool { return false }

func mmapFile(_ *os.File) (*mmapMapping, error) { return nil, errMmapUnsupported }
