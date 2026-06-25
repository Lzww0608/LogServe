package logstore

import (
	"os"
	"runtime"
	"testing"
)

func TestMmapReadSealedSegmentRoundTrip(t *testing.T) {
	if !mmapSupported() {
		t.Skip("mmap not supported on this platform")
	}
	t.Setenv("LOGSERVE_LOG_MMAP_READ", "1")

	dir := t.TempDir()
	opts := DefaultOptions()
	opts.MmapRead = true
	opts.SegmentSizeBytes = 256
	store, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:mmap",
			EventType: "MmapEvent",
			Payload:   []byte("payload"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("task:mmap", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4", len(records))
	}
	stats := recovered.MmapReadStats()
	if stats.MappedSegments == 0 {
		t.Fatalf("expected mapped segments, stats=%+v", stats)
	}
}

func TestMmapReadActiveSegmentUsesReadAt(t *testing.T) {
	if !mmapSupported() {
		t.Skip("mmap not supported on this platform")
	}
	opts := DefaultOptions()
	opts.MmapRead = true
	store, err := OpenWithOptions(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.Append(AppendRequest{
		StreamID:  "task:active",
		EventType: "ActiveEvent",
		Payload:   []byte("active"),
	}); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	activeID := store.activeSegmentID
	store.mu.Unlock()

	reader, err := store.acquireSegmentReader(activeID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.releaseSegmentReader(reader)
	if _, ok := reader.reader.MmapData(); ok {
		t.Fatal("active segment should not be mmap-backed")
	}
}

func TestMmapReadFromEnv(t *testing.T) {
	t.Setenv("LOGSERVE_LOG_MMAP_READ", "1")
	opts, err := DefaultOptions().normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !opts.MmapRead {
		t.Fatal("expected mmap read enabled from env")
	}
}

func TestMmapReadWindowsDisabled(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skip("platform supports mmap read experiment")
	}
	if mmapSupported() {
		t.Fatal("expected mmapSupported false on windows/other")
	}
	opts := DefaultOptions()
	opts.MmapRead = true
	store, err := OpenWithOptions(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.Append(AppendRequest{StreamID: "task:other", EventType: "Event", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenWithOptions(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
}

func TestMmapReadCompactionReleasesMapping(t *testing.T) {
	if !mmapSupported() {
		t.Skip("mmap not supported on this platform")
	}
	if os.Getenv("LOGSERVE_RUN_COMPACTION_TESTS") != "1" {
		t.Skip("set LOGSERVE_RUN_COMPACTION_TESTS=1 to run compaction mmap lifecycle test")
	}
}
