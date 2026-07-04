package logstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const compactionManifestFileName = "compaction-manifest.json"

// CompactionOptions controls which reclamation modes run and how aggressively
// copy-compaction writes new segment bytes.
type CompactionOptions struct {
	DeleteFullyTrimmedSegments bool
	CopyPartialSegments        bool
	CopyLiveRatioThreshold     float64
	MaxBytesPerSecond          int64
}

// SegmentCompactionStats describes how much of one segment is still live under
// the current per-stream trim watermarks.
type SegmentCompactionStats struct {
	SegmentID          uint64
	Active             bool
	TotalBytes         uint64
	LiveBytes          uint64
	CompactableBytes   uint64
	TotalRecords       uint64
	LiveRecords        uint64
	CompactableRecords uint64
	FullyCompactable   bool
	Streams            []SegmentStreamCompactionStats
}

// SegmentStreamCompactionStats is the per-stream breakdown nested inside a
// segment-level compactability report.
type SegmentStreamCompactionStats struct {
	StreamID           string
	MinSeq             uint64
	MaxSeq             uint64
	TrimmedBeforeSeq   uint64
	TotalBytes         uint64
	LiveBytes          uint64
	CompactableBytes   uint64
	TotalRecords       uint64
	LiveRecords        uint64
	CompactableRecords uint64
}

// CompactionResult reports what a compaction attempt changed and includes the
// stats snapshot used to make the compaction decision.
type CompactionResult struct {
	CompactionID     string
	DeletedSegments  []uint64
	CopiedSegments   []CompactedSegment
	ReclaimedBytes   uint64
	RewrittenBytes   uint64
	CompactableStats []SegmentCompactionStats
}

// CompactedSegment records the old-to-new segment mapping produced by copying
// live records out of a partially trimmed segment.
type CompactedSegment struct {
	OldSegmentID uint64
	NewSegmentID uint64
	TotalBytes   uint64
	LiveBytes    uint64
}

// compactionManifest is the crash-recovery marker written before deleting or
// replacing segment files. Recovery replays the manifest before rebuilding
// indexes so disk and in-memory state converge.
type compactionManifest struct {
	CompactionID    string                   `json:"compaction_id"`
	DeletedSegments []uint64                 `json:"deleted_segments"`
	CopiedSegments  []compactionManifestCopy `json:"copied_segments,omitempty"`
	SafeBefore      map[string]uint64        `json:"safe_before"`
	StartedAtMs     int64                    `json:"started_at_ms"`
	CompletedAtMs   int64                    `json:"completed_at_ms,omitempty"`
}

// compactionManifestCopy stores one planned segment replacement in the
// manifest so recovery can finish or roll back temporary files.
type compactionManifestCopy struct {
	OldSegmentID uint64 `json:"old_segment_id"`
	NewSegmentID uint64 `json:"new_segment_id"`
}

// segmentCopyPlan holds the live entries that will be rewritten from an old
// segment into a newly allocated segment ID.
type segmentCopyPlan struct {
	oldSegmentID uint64
	newSegmentID uint64
	stats        SegmentCompactionStats
	entries      []recoveredIndexEntry
}

// segmentStatsBuilder accumulates per-segment and per-stream stats before the
// public compactability slice is sorted and cloned.
type segmentStatsBuilder struct {
	stats   SegmentCompactionStats
	streams map[string]*SegmentStreamCompactionStats
}

// startBackgroundCompactor launches the optional periodic compaction loop. The
// loop is owned by Store and stopped by Close through the stored cancel func.
func (s *Store) startBackgroundCompactor() {
	if s.options.CompactionInterval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.compactorCancel = cancel
	s.compactorDone = done
	interval := s.options.CompactionInterval
	opts := CompactionOptions{
		DeleteFullyTrimmedSegments: true,
		CopyPartialSegments:        true,
		CopyLiveRatioThreshold:     s.options.CompactionCopyLiveRatioThreshold,
		MaxBytesPerSecond:          s.options.CompactionMaxBytesPerSecond,
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.Compact(ctx, opts)
			}
		}
	}()
}

// stopBackgroundCompactor cancels the periodic compaction loop and waits for
// it to exit before clearing lifecycle fields.
func (s *Store) stopBackgroundCompactor() {
	if s.compactorCancel == nil {
		return
	}
	s.compactorCancel()
	if s.compactorDone != nil {
		<-s.compactorDone
	}
	s.compactorCancel = nil
	s.compactorDone = nil
}

// DefaultCompactionOptions returns the foreground compaction defaults used when
// callers do not explicitly select a mode.
func DefaultCompactionOptions() CompactionOptions {
	return CompactionOptions{
		DeleteFullyTrimmedSegments: true,
		CopyPartialSegments:        true,
		CopyLiveRatioThreshold:     defaultCompactionCopyLiveRatio,
		MaxBytesPerSecond:          defaultCompactionMaxBytesPerSecond,
	}
}

// normalize fills compaction defaults and validates thresholds before a Store
// lock is taken for the actual compaction pass.
func (opts CompactionOptions) normalize() (CompactionOptions, error) {
	if !opts.DeleteFullyTrimmedSegments && !opts.CopyPartialSegments {
		defaults := DefaultCompactionOptions()
		if opts.CopyLiveRatioThreshold != 0 {
			defaults.CopyLiveRatioThreshold = opts.CopyLiveRatioThreshold
		}
		if opts.MaxBytesPerSecond != 0 {
			defaults.MaxBytesPerSecond = opts.MaxBytesPerSecond
		}
		opts = defaults
	}
	if opts.CopyLiveRatioThreshold == 0 {
		opts.CopyLiveRatioThreshold = defaultCompactionCopyLiveRatio
	}
	if opts.CopyLiveRatioThreshold < 0 || opts.CopyLiveRatioThreshold > 1 {
		return CompactionOptions{}, errors.New("compaction copy live-ratio threshold must be between 0 and 1")
	}
	if opts.MaxBytesPerSecond < 0 {
		return CompactionOptions{}, errors.New("compaction max bytes per second cannot be negative")
	}
	if opts.MaxBytesPerSecond == 0 {
		opts.MaxBytesPerSecond = defaultCompactionMaxBytesPerSecond
	}
	return opts, nil
}

// CompactabilityStats returns a cloned snapshot so callers cannot mutate the
// store's internal stream and segment accounting.
func (s *Store) CompactabilityStats() []SegmentCompactionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSegmentCompactionStats(s.compactabilityStatsLocked())
}

// Compact reclaims bytes made obsolete by logical trims. It may delete fully
// trimmed sealed segments or copy live records out of sparse sealed segments;
// active segments and busy cached readers are never modified.
func (s *Store) Compact(ctx context.Context, opts CompactionOptions) (CompactionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := opts.normalize()
	if err != nil {
		return CompactionResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return CompactionResult{}, err
	}

	stats := s.compactabilityStatsLocked()
	result := CompactionResult{CompactableStats: cloneSegmentCompactionStats(stats)}
	deleteIDs, copyPlans := s.compactionPlansLocked(stats, normalized)
	if len(deleteIDs) == 0 && len(copyPlans) == 0 {
		return result, nil
	}

	maxSegmentID, err := maxSegmentIDOnDisk(s.dir)
	if err != nil {
		return CompactionResult{}, err
	}
	nextSegmentID := maxSegmentID + 1
	for i := range copyPlans {
		copyPlans[i].newSegmentID = nextSegmentID
		nextSegmentID++
	}

	manifest := compactionManifest{
		CompactionID:    newCompactionID(),
		DeletedSegments: append([]uint64(nil), deleteIDs...),
		SafeBefore:      s.compactionSafeBeforeLocked(),
		StartedAtMs:     time.Now().UnixMilli(),
	}
	for _, plan := range copyPlans {
		manifest.CopiedSegments = append(manifest.CopiedSegments, compactionManifestCopy{
			OldSegmentID: plan.oldSegmentID,
			NewSegmentID: plan.newSegmentID,
		})
	}
	// The manifest is durable before any file removal or replacement so recovery
	// can complete the same plan after a crash.
	if err := s.writeCompactionManifestLocked(manifest); err != nil {
		return CompactionResult{}, err
	}
	result.CompactionID = manifest.CompactionID

	for _, segmentID := range deleteIDs {
		if err := s.closeSegmentReaderLocked(segmentID); err != nil {
			return CompactionResult{}, err
		}
		if err := removeSegmentFilesOnDisk(s.dir, segmentID); err != nil {
			return CompactionResult{}, err
		}
		result.DeletedSegments = append(result.DeletedSegments, segmentID)
	}
	if len(deleteIDs) > 0 {
		s.applyCompactionIndexChangesLocked(deleteIDs, nil)
		if err := s.rebuildIdempotencyLocked(); err != nil {
			return CompactionResult{}, err
		}
	}

	for i := range copyPlans {
		if err := s.writeCompactedSegmentLocked(ctx, &copyPlans[i], normalized); err != nil {
			return CompactionResult{}, err
		}
	}
	if len(copyPlans) > 0 {
		s.applyCompactionIndexChangesLocked(nil, copyPlans)
		if err := s.rebuildIdempotencyLocked(); err != nil {
			return CompactionResult{}, err
		}
	}
	for _, plan := range copyPlans {
		if err := s.closeSegmentReaderLocked(plan.oldSegmentID); err != nil {
			return CompactionResult{}, err
		}
		if err := removeSegmentFilesOnDisk(s.dir, plan.oldSegmentID); err != nil {
			return CompactionResult{}, err
		}
		result.CopiedSegments = append(result.CopiedSegments, CompactedSegment{
			OldSegmentID: plan.oldSegmentID,
			NewSegmentID: plan.newSegmentID,
			TotalBytes:   plan.stats.TotalBytes,
			LiveBytes:    plan.stats.LiveBytes,
		})
	}
	if len(copyPlans) > 0 {
		if err := s.closeActiveFilesLocked(true); err != nil {
			return CompactionResult{}, err
		}
		s.activeSegmentID = nextSegmentID
		s.activeSegmentBytes = 0
		if err := s.openActiveFilesLocked(); err != nil {
			return CompactionResult{}, err
		}
	}
	for _, segmentID := range deleteIDs {
		if stat := segmentStatByID(stats, segmentID); stat != nil {
			result.ReclaimedBytes += stat.TotalBytes
		}
	}
	for _, plan := range copyPlans {
		result.ReclaimedBytes += plan.stats.TotalBytes - plan.stats.LiveBytes
		result.RewrittenBytes += plan.stats.LiveBytes
	}

	// A second manifest write marks the plan complete after all in-memory and disk
	// changes have been applied; recovery still treats the manifest as replayable.
	manifest.CompletedAtMs = time.Now().UnixMilli()
	if err := s.writeCompactionManifestLocked(manifest); err != nil {
		return CompactionResult{}, err
	}
	return result, syncDirBestEffort(s.dir)
}

// compactabilityStatsLocked derives segment liveness from stream indexes and
// trim watermarks. The caller must hold s.mu.
func (s *Store) compactabilityStatsLocked() []SegmentCompactionStats {
	builders := make(map[uint64]*segmentStatsBuilder)
	for streamID, state := range s.streams {
		trimBefore := state.trimBefore
		for _, entry := range state.entries {
			builder := builders[entry.SegmentID]
			if builder == nil {
				builder = &segmentStatsBuilder{
					stats: SegmentCompactionStats{
						SegmentID: entry.SegmentID,
						Active:    entry.SegmentID == s.activeSegmentID,
					},
					streams: make(map[string]*SegmentStreamCompactionStats),
				}
				builders[entry.SegmentID] = builder
			}
			builder.stats.TotalRecords++
			builder.stats.TotalBytes += uint64(entry.Length)

			streamStats := builder.streams[streamID]
			if streamStats == nil {
				streamStats = &SegmentStreamCompactionStats{
					StreamID:         streamID,
					MinSeq:           entry.Seq,
					MaxSeq:           entry.Seq,
					TrimmedBeforeSeq: trimBefore,
				}
				builder.streams[streamID] = streamStats
			}
			if entry.Seq < streamStats.MinSeq {
				streamStats.MinSeq = entry.Seq
			}
			if entry.Seq > streamStats.MaxSeq {
				streamStats.MaxSeq = entry.Seq
			}
			streamStats.TotalRecords++
			streamStats.TotalBytes += uint64(entry.Length)

			if trimBefore > 0 && entry.Seq < trimBefore {
				builder.stats.CompactableRecords++
				builder.stats.CompactableBytes += uint64(entry.Length)
				streamStats.CompactableRecords++
				streamStats.CompactableBytes += uint64(entry.Length)
				continue
			}
			builder.stats.LiveRecords++
			builder.stats.LiveBytes += uint64(entry.Length)
			streamStats.LiveRecords++
			streamStats.LiveBytes += uint64(entry.Length)
		}
	}

	ids := make([]uint64, 0, len(builders))
	for segmentID := range builders {
		ids = append(ids, segmentID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]SegmentCompactionStats, 0, len(ids))
	for _, segmentID := range ids {
		builder := builders[segmentID]
		builder.stats.FullyCompactable = builder.stats.TotalRecords > 0 && builder.stats.LiveRecords == 0
		streamIDs := make([]string, 0, len(builder.streams))
		for streamID := range builder.streams {
			streamIDs = append(streamIDs, streamID)
		}
		sort.Strings(streamIDs)
		for _, streamID := range streamIDs {
			builder.stats.Streams = append(builder.stats.Streams, *builder.streams[streamID])
		}
		out = append(out, builder.stats)
	}
	return out
}

// compactionPlansLocked converts stats into delete and copy plans while
// avoiding active segments and segments with outstanding readers.
func (s *Store) compactionPlansLocked(stats []SegmentCompactionStats, opts CompactionOptions) ([]uint64, []segmentCopyPlan) {
	deleteIDs := make([]uint64, 0)
	copyPlans := make([]segmentCopyPlan, 0)
	for _, stat := range stats {
		if stat.Active || stat.TotalRecords == 0 || s.segmentReaderBusyLocked(stat.SegmentID) {
			continue
		}
		if opts.DeleteFullyTrimmedSegments && stat.FullyCompactable {
			deleteIDs = append(deleteIDs, stat.SegmentID)
			continue
		}
		if !opts.CopyPartialSegments || stat.CompactableRecords == 0 || stat.LiveRecords == 0 || stat.TotalBytes == 0 {
			continue
		}
		liveRatio := float64(stat.LiveBytes) / float64(stat.TotalBytes)
		if liveRatio > opts.CopyLiveRatioThreshold {
			continue
		}
		copyPlans = append(copyPlans, segmentCopyPlan{
			oldSegmentID: stat.SegmentID,
			stats:        stat,
			entries:      s.liveEntriesForSegmentLocked(stat.SegmentID),
		})
	}
	return deleteIDs, copyPlans
}

// liveEntriesForSegmentLocked returns the untrimmed records in physical offset
// order so copy-compaction preserves on-disk append order within the segment.
func (s *Store) liveEntriesForSegmentLocked(segmentID uint64) []recoveredIndexEntry {
	entries := make([]recoveredIndexEntry, 0)
	for streamID, state := range s.streams {
		for _, entry := range state.entries {
			if entry.SegmentID != segmentID {
				continue
			}
			if state.trimBefore > 0 && entry.Seq < state.trimBefore {
				continue
			}
			entries = append(entries, recoveredIndexEntry{streamID: streamID, entry: entry})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].entry.Offset < entries[j].entry.Offset })
	return entries
}

// applyCompactionIndexChangesLocked removes deleted/replaced entries and inserts
// rewritten entries, then re-sorts each stream by sequence.
func (s *Store) applyCompactionIndexChangesLocked(deletedIDs []uint64, copyPlans []segmentCopyPlan) {
	removed := make(map[uint64]struct{}, len(deletedIDs)+len(copyPlans))
	for _, segmentID := range deletedIDs {
		removed[segmentID] = struct{}{}
	}
	for _, plan := range copyPlans {
		removed[plan.oldSegmentID] = struct{}{}
	}
	newEntries := make(map[string][]streamIndexEntry)
	for _, plan := range copyPlans {
		for _, item := range plan.entries {
			newEntries[item.streamID] = append(newEntries[item.streamID], item.entry)
		}
	}
	for streamID, state := range s.streams {
		kept := state.entries[:0]
		for _, entry := range state.entries {
			if _, ok := removed[entry.SegmentID]; ok {
				continue
			}
			kept = append(kept, entry)
		}
		state.entries = append(kept, newEntries[streamID]...)
		sort.Slice(state.entries, func(i, j int) bool { return state.entries[i].Seq < state.entries[j].Seq })
		if state.nextSeq < state.trimBefore {
			state.nextSeq = state.trimBefore
		}
	}
}

// writeCompactedSegmentLocked copies live records into temporary segment files,
// fsyncs them, and atomically renames them into place. The caller holds s.mu.
func (s *Store) writeCompactedSegmentLocked(ctx context.Context, plan *segmentCopyPlan, opts CompactionOptions) (err error) {
	logTmp := compactTempPath(s.dir, plan.newSegmentID, ".log")
	indexTmp := compactTempPath(s.dir, plan.newSegmentID, ".index")
	logFinal := segmentPath(s.dir, plan.newSegmentID, ".log")
	indexFinal := segmentPath(s.dir, plan.newSegmentID, ".index")
	// cleanup remains true until both temp files have been renamed; this keeps
	// partial copies from being treated as valid segments after an error.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(logTmp)
			_ = os.Remove(indexTmp)
			_ = os.Remove(logFinal)
			_ = os.Remove(indexFinal)
		}
	}()

	oldFile, err := os.Open(segmentPath(s.dir, plan.oldSegmentID, ".log"))
	if err != nil {
		return err
	}
	defer oldFile.Close()

	newFile, err := os.OpenFile(logTmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var offset uint64
	newEntries := make([]recoveredIndexEntry, 0, len(plan.entries))
	for _, item := range plan.entries {
		if err := ctx.Err(); err != nil {
			_ = newFile.Close()
			return err
		}
		rec, nextOffset, err := readRecordAt(oldFile, int64(item.entry.Offset))
		if err != nil {
			_ = newFile.Close()
			return err
		}
		// Re-read validation fences against stale or corrupt index entries before the
		// compacted segment becomes durable.
		if rec.StreamID != item.streamID || rec.Seq != item.entry.Seq || nextOffset-int64(item.entry.Offset) != int64(item.entry.Length) {
			_ = newFile.Close()
			return errCorruptRecord
		}
		encoded, _, err := encodeRecordPooled(rec, rec.ChecksumType)
		if err != nil {
			_ = newFile.Close()
			return err
		}
		encodedLen := len(encoded)
		if _, err := newFile.Write(encoded); err != nil {
			putRecordEncodeBuffer(encoded)
			_ = newFile.Close()
			return err
		}
		putRecordEncodeBuffer(encoded)
		entry := item.entry
		entry.SegmentID = plan.newSegmentID
		entry.Offset = offset
		entry.Length = uint32(encodedLen)
		newEntries = append(newEntries, recoveredIndexEntry{streamID: item.streamID, entry: entry})
		offset += uint64(encodedLen)
		if err := throttleCompactionWrite(ctx, int64(encodedLen), opts.MaxBytesPerSecond); err != nil {
			_ = newFile.Close()
			return err
		}
	}
	if err := errors.Join(newFile.Sync(), newFile.Close()); err != nil {
		return err
	}

	indexFile, err := os.OpenFile(indexTmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := writeSegmentIndex(indexFile, plan.newSegmentID, newEntries); err != nil {
		_ = indexFile.Close()
		return err
	}
	if err := errors.Join(indexFile.Sync(), indexFile.Close()); err != nil {
		return err
	}
	if err := os.Rename(logTmp, logFinal); err != nil {
		return err
	}
	if err := os.Rename(indexTmp, indexFinal); err != nil {
		return err
	}
	plan.entries = newEntries
	cleanup = false
	return syncDirBestEffort(s.dir)
}

// rebuildIdempotencyLocked reconstructs duplicate-detection state from the
// surviving index entries after compaction changes segment membership.
func (s *Store) rebuildIdempotencyLocked() error {
	bySegment := make(map[uint64][]recoveredIndexEntry)
	for streamID, state := range s.streams {
		for _, entry := range state.entries {
			bySegment[entry.SegmentID] = append(bySegment[entry.SegmentID], recoveredIndexEntry{streamID: streamID, entry: entry})
		}
	}
	segmentIDs := make([]uint64, 0, len(bySegment))
	for segmentID := range bySegment {
		segmentIDs = append(segmentIDs, segmentID)
	}
	sort.Slice(segmentIDs, func(i, j int) bool { return segmentIDs[i] < segmentIDs[j] })

	idempotency := make(map[string]Record)
	for _, segmentID := range segmentIDs {
		items := bySegment[segmentID]
		sort.Slice(items, func(i, j int) bool { return items[i].entry.Offset < items[j].entry.Offset })
		file, err := os.Open(segmentPath(s.dir, segmentID, ".log"))
		if err != nil {
			return err
		}
		for _, item := range items {
			rec, err := readIndexedRecordFromFile(file, item.streamID, item.entry)
			if err != nil {
				_ = file.Close()
				return err
			}
			if rec.IdempotencyKey != "" {
				idempotency[idempotencyKey(rec.StreamID, rec.IdempotencyKey)] = idempotencyRecord(rec)
			}
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	s.idempotency = idempotency
	return nil
}

// compactionSafeBeforeLocked snapshots trim watermarks into the manifest for
// observability and crash-recovery auditing.
func (s *Store) compactionSafeBeforeLocked() map[string]uint64 {
	out := make(map[string]uint64)
	for streamID, state := range s.streams {
		if state.trimBefore > 0 {
			out[streamID] = state.trimBefore
		}
	}
	return out
}

// writeCompactionManifestLocked persists the manifest through a temp file and
// rename so recovery never observes a partially written JSON document.
func (s *Store) writeCompactionManifestLocked(manifest compactionManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, compactionManifestFileName)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncDirBestEffort(s.dir)
}

// closeSegmentReaderLocked evicts and closes a cached reader when no active
// ReadEach/ReadRawEach callback is using it.
func (s *Store) closeSegmentReaderLocked(segmentID uint64) error {
	reader := s.segmentReaders[segmentID]
	if reader == nil {
		return nil
	}
	if reader.refs > 0 {
		return fmt.Errorf("segment %d has active readers", segmentID)
	}
	if reader.element != nil {
		s.segmentReaderLRU.Remove(reader.element)
	}
	delete(s.segmentReaders, segmentID)
	reader.cached = false
	reader.element = nil
	return reader.reader.Close()
}

// segmentReaderBusyLocked reports whether a segment has outstanding reader refs
// and therefore cannot be safely deleted or replaced.
func (s *Store) segmentReaderBusyLocked(segmentID uint64) bool {
	reader := s.segmentReaders[segmentID]
	return reader != nil && reader.refs > 0
}

// reconcileCompactionManifestBeforeRecover completes any recorded compaction
// before normal log recovery rebuilds indexes from the remaining files.
func reconcileCompactionManifestBeforeRecover(dir string) error {
	manifest, ok, err := readCompactionManifest(dir)
	if err != nil || !ok {
		return err
	}
	for _, copied := range manifest.CopiedSegments {
		if err := reconcileCopiedSegmentBeforeRecover(dir, copied); err != nil {
			return err
		}
	}
	for _, segmentID := range manifest.DeletedSegments {
		if err := removeSegmentFilesOnDisk(dir, segmentID); err != nil {
			return err
		}
	}
	return syncDirBestEffort(dir)
}

// readCompactionManifest loads the optional recovery marker from disk.
func readCompactionManifest(dir string) (compactionManifest, bool, error) {
	path := filepath.Join(dir, compactionManifestFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return compactionManifest{}, false, nil
	}
	if err != nil {
		return compactionManifest{}, false, err
	}
	var manifest compactionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return compactionManifest{}, false, err
	}
	return manifest, true, nil
}

// reconcileCopiedSegmentBeforeRecover finishes the temp-file rename sequence or
// removes an incomplete copy when the old segment is still available.
func reconcileCopiedSegmentBeforeRecover(dir string, copied compactionManifestCopy) error {
	oldExists := fileExists(segmentPath(dir, copied.OldSegmentID, ".log"))
	logFinal := segmentPath(dir, copied.NewSegmentID, ".log")
	indexFinal := segmentPath(dir, copied.NewSegmentID, ".index")
	logTmp := compactTempPath(dir, copied.NewSegmentID, ".log")
	indexTmp := compactTempPath(dir, copied.NewSegmentID, ".index")
	logFinalExists := fileExists(logFinal)
	indexFinalExists := fileExists(indexFinal)
	logTmpExists := fileExists(logTmp)
	indexTmpExists := fileExists(indexTmp)

	// A log temp file is promoted first only when both temp files exist, preserving
	// the invariant that a final log is never accepted without an index path.
	if !logFinalExists && logTmpExists && indexTmpExists {
		if err := os.Rename(logTmp, logFinal); err != nil {
			return err
		}
		logFinalExists = true
		logTmpExists = false
	}
	if !indexFinalExists && indexTmpExists && logFinalExists {
		if err := os.Rename(indexTmp, indexFinal); err != nil {
			return err
		}
		indexFinalExists = true
		indexTmpExists = false
	}
	// Once both final files exist, deleting the old segment completes the copy plan.
	if logFinalExists && indexFinalExists {
		return removeSegmentFilesOnDisk(dir, copied.OldSegmentID)
	}
	if oldExists {
		_ = os.Remove(logTmp)
		_ = os.Remove(indexTmp)
		_ = os.Remove(logFinal)
		_ = os.Remove(indexFinal)
		return nil
	}
	if logTmpExists || indexTmpExists || logFinalExists || indexFinalExists {
		return fmt.Errorf("incomplete compacted segment %d without old segment %d", copied.NewSegmentID, copied.OldSegmentID)
	}
	return nil
}

// removeSegmentFilesOnDisk deletes both index and log files and ignores missing
// files so repeated recovery passes stay idempotent.
func removeSegmentFilesOnDisk(dir string, segmentID uint64) error {
	var err error
	for _, ext := range []string{".index", ".log"} {
		path := segmentPath(dir, segmentID, ext)
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

// compactTempPath returns the non-discoverable temp filename used during segment
// copy. The suffix prevents discoverSegmentIDs from treating it as live data.
func compactTempPath(dir string, segmentID uint64, ext string) string {
	return filepath.Join(dir, fmt.Sprintf("segment-%08d%s.compact.tmp", segmentID, ext))
}

// maxSegmentIDOnDisk returns the highest existing log segment ID so new compacted
// segments are allocated above every currently discoverable segment.
func maxSegmentIDOnDisk(dir string) (uint64, error) {
	segmentIDs, err := discoverSegmentIDs(dir, ".log")
	if err != nil || len(segmentIDs) == 0 {
		return 0, err
	}
	return segmentIDs[len(segmentIDs)-1], nil
}

// segmentStatByID returns the stats entry used to compute reclaimed bytes for a
// deleted segment.
func segmentStatByID(stats []SegmentCompactionStats, segmentID uint64) *SegmentCompactionStats {
	for i := range stats {
		if stats[i].SegmentID == segmentID {
			return &stats[i]
		}
	}
	return nil
}

// cloneSegmentCompactionStats performs a shallow copy plus per-segment stream
// slice copies for safe caller ownership.
func cloneSegmentCompactionStats(stats []SegmentCompactionStats) []SegmentCompactionStats {
	out := append([]SegmentCompactionStats(nil), stats...)
	for i := range out {
		out[i].Streams = append([]SegmentStreamCompactionStats(nil), out[i].Streams...)
	}
	return out
}

// throttleCompactionWrite sleeps after each copied record to approximate the
// configured byte-per-second limit while remaining cancelable through ctx.
func throttleCompactionWrite(ctx context.Context, bytesWritten, maxBytesPerSecond int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bytesWritten <= 0 || maxBytesPerSecond <= 0 {
		return nil
	}
	delay := time.Duration(bytesWritten) * time.Second / time.Duration(maxBytesPerSecond)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newCompactionID creates a human-readable unique-enough ID for local manifest
// diagnostics; it is not used as a distributed coordination token.
func newCompactionID() string {
	return fmt.Sprintf("compact-%d", time.Now().UnixNano())
}

// fileExists reports whether a path is present and readable by Stat.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// syncDirBestEffort asks the filesystem to persist directory entry changes.
// Some platforms reject directory sync, so failure to open the directory is
// treated as non-fatal.
func syncDirBestEffort(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer file.Close()
	_ = file.Sync()
	return nil
}
