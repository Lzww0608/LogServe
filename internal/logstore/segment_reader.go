package logstore

import (
	"errors"
	"io"
	"os"
	"sync/atomic"
)

var errMmapUnsupported = errors.New("mmap read is not supported on this platform")

// SegmentReader exposes sealed-segment mmap and active-segment ReadAt access.
type SegmentReader interface {
	SegmentID() uint64
	MmapData() ([]byte, bool)
	ReadAt(p []byte, off int64) (int, error)
	Close() error
}

type readAtSegmentReader struct {
	segmentID uint64
	file      *os.File
}

func (r *readAtSegmentReader) SegmentID() uint64 { return r.segmentID }

func (r *readAtSegmentReader) MmapData() ([]byte, bool) { return nil, false }

func (r *readAtSegmentReader) ReadAt(p []byte, off int64) (int, error) {
	return r.file.ReadAt(p, off)
}

func (r *readAtSegmentReader) Close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

type mmapSegmentReader struct {
	segmentID uint64
	file      *os.File
	mmap      *mmapMapping
	data      []byte
}

func (r *mmapSegmentReader) SegmentID() uint64 { return r.segmentID }

func (r *mmapSegmentReader) MmapData() ([]byte, bool) {
	if len(r.data) == 0 {
		return nil, false
	}
	return r.data, true
}

func (r *mmapSegmentReader) ReadAt(p []byte, off int64) (int, error) {
	if len(r.data) > 0 {
		start := int(off)
		end := start + len(p)
		if start < 0 || end > len(r.data) {
			return 0, io.EOF
		}
		return copy(p, r.data[start:end]), nil
	}
	return r.file.ReadAt(p, off)
}

func (r *mmapSegmentReader) Close() error {
	var err error
	if r.mmap != nil {
		err = errors.Join(err, r.mmap.Close())
		r.mmap = nil
		r.data = nil
	}
	if r.file != nil {
		err = errors.Join(err, r.file.Close())
		r.file = nil
	}
	return err
}

type segmentIO struct {
	reader SegmentReader
}

func (io *segmentIO) ReadAt(p []byte, off int64) (int, error) {
	return io.reader.ReadAt(p, off)
}

func (io *segmentIO) slice(off int64, length int) ([]byte, bool) {
	data, ok := io.reader.MmapData()
	if !ok {
		return nil, false
	}
	start := int(off)
	end := start + length
	if start < 0 || end > len(data) {
		return nil, false
	}
	return data[start:end], true
}

type MmapReadStats struct {
	MappedSegments uint64
	MappedBytes    uint64
	ReadAtSegments uint64
}

var (
	mmapMappedSegments atomic.Uint64
	mmapMappedBytes    atomic.Uint64
	mmapReadAtSegments atomic.Uint64
)

func (s *Store) MmapReadStats() MmapReadStats {
	return MmapReadStats{
		MappedSegments: mmapMappedSegments.Load(),
		MappedBytes:    mmapMappedBytes.Load(),
		ReadAtSegments: mmapReadAtSegments.Load(),
	}
}

func recordSegmentOpen(mapped bool, mappedBytes int64) {
	if mapped {
		mmapMappedSegments.Add(1)
		if mappedBytes > 0 {
			mmapMappedBytes.Add(uint64(mappedBytes))
		}
		return
	}
	mmapReadAtSegments.Add(1)
}

func openSegmentReader(dir string, segmentID uint64, sealed bool, mmapRead bool) (SegmentReader, error) {
	file, err := os.Open(segmentPath(dir, segmentID, ".log"))
	if err != nil {
		return nil, err
	}
	if mmapRead && sealed && mmapSupported() {
		mapping, err := mmapFile(file)
		if err == nil {
			recordSegmentOpen(true, int64(len(mapping.data)))
			return &mmapSegmentReader{
				segmentID: segmentID,
				file:      file,
				mmap:      mapping,
				data:      mapping.data,
			}, nil
		}
	}
	recordSegmentOpen(false, 0)
	return &readAtSegmentReader{segmentID: segmentID, file: file}, nil
}
