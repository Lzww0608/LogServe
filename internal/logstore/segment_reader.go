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

// readAtSegmentReader is the portable reader implementation used for active
// segments and platforms where mmap is disabled or unsupported.
type readAtSegmentReader struct {
	segmentID uint64
	file      *os.File
}

// SegmentID returns the segment this reader was opened for.
func (r *readAtSegmentReader) SegmentID() uint64 { return r.segmentID }

// MmapData reports that ReadAt-backed readers do not expose a direct slice.
func (r *readAtSegmentReader) MmapData() ([]byte, bool) { return nil, false }

// ReadAt delegates random access to the underlying segment file.
func (r *readAtSegmentReader) ReadAt(p []byte, off int64) (int, error) {
	return r.file.ReadAt(p, off)
}

// Close releases the underlying file descriptor and tolerates repeated calls.
func (r *readAtSegmentReader) Close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// mmapSegmentReader wraps a sealed segment mapped into memory. It keeps the
// file descriptor open for the lifetime of the mapping and falls back to ReadAt
// when a zero-length mapping is returned.
type mmapSegmentReader struct {
	segmentID uint64
	file      *os.File
	mmap      *mmapMapping
	data      []byte
}

// SegmentID returns the segment this mmap reader was opened for.
func (r *mmapSegmentReader) SegmentID() uint64 { return r.segmentID }

// MmapData exposes the mapped bytes when the mapping is non-empty.
func (r *mmapSegmentReader) MmapData() ([]byte, bool) {
	if len(r.data) == 0 {
		return nil, false
	}
	return r.data, true
}

// ReadAt copies from the mmap slice when possible and preserves os.File.ReadAt
// semantics by returning io.EOF for out-of-range reads.
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

// Close unmaps memory before closing the file so the platform mapping lifetime
// is shorter than the file descriptor lifetime.
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

// segmentIO is the minimal read adapter used by shared record decoding paths.
// It lets the decoder choose between direct mmap slicing and ordinary ReadAt.
type segmentIO struct {
	reader SegmentReader
}

// ReadAt forwards to the underlying SegmentReader.
func (io *segmentIO) ReadAt(p []byte, off int64) (int, error) {
	return io.reader.ReadAt(p, off)
}

// slice returns a bounded subslice from an mmap-backed reader. The boolean is
// false when the reader has no mapping or the requested range is invalid.
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

// MmapReadStats exposes process-wide counters for opened mmap and ReadAt
// segment readers. The counters are diagnostic and intentionally monotonic.
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

// MmapReadStats returns the current process-wide mmap/readAt open counters.
func (s *Store) MmapReadStats() MmapReadStats {
	return MmapReadStats{
		MappedSegments: mmapMappedSegments.Load(),
		MappedBytes:    mmapMappedBytes.Load(),
		ReadAtSegments: mmapReadAtSegments.Load(),
	}
}

// recordSegmentOpen updates diagnostic counters for one opened segment reader.
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

// openSegmentReader opens a segment using mmap only for sealed segments. Active
// segments stay on ReadAt because appends can extend them while readers are
// active.
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
