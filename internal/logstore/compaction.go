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

type CompactionOptions struct {
	DeleteFullyTrimmedSegments bool
	CopyPartialSegments        bool
	CopyLiveRatioThreshold     float64
	MaxBytesPerSecond          int64
}

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

type CompactionResult struct {
	CompactionID     string
	DeletedSegments  []uint64
	CopiedSegments   []CompactedSegment
	ReclaimedBytes   uint64
	RewrittenBytes   uint64
	CompactableStats []SegmentCompactionStats
}

type CompactedSegment struct {
	OldSegmentID uint64
	NewSegmentID uint64
	TotalBytes   uint64
	LiveBytes    uint64
}

type compactionManifest struct {
	CompactionID    string                   `json:"compaction_id"`
	DeletedSegments []uint64                 `json:"deleted_segments"`
	CopiedSegments  []compactionManifestCopy `json:"copied_segments,omitempty"`
	SafeBefore      map[string]uint64        `json:"safe_before"`
	StartedAtMs     int64                    `json:"started_at_ms"`
	CompletedAtMs   int64                    `json:"completed_at_ms,omitempty"`
}

type compactionManifestCopy struct {
	OldSegmentID uint64 `json:"old_segment_id"`
	NewSegmentID uint64 `json:"new_segment_id"`
}

type segmentCopyPlan struct {
	oldSegmentID uint64
	newSegmentID uint64
	stats        SegmentCompactionStats
	entries      []recoveredIndexEntry
}

type segmentStatsBuilder struct {
	stats   SegmentCompactionStats
	streams map[string]*SegmentStreamCompactionStats
}

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

func DefaultCompactionOptions() CompactionOptions {
	return CompactionOptions{
		DeleteFullyTrimmedSegments: true,
		CopyPartialSegments:        true,
		CopyLiveRatioThreshold:     defaultCompactionCopyLiveRatio,
		MaxBytesPerSecond:          defaultCompactionMaxBytesPerSecond,
	}
}

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

func (s *Store) CompactabilityStats() []SegmentCompactionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSegmentCompactionStats(s.compactabilityStatsLocked())
}

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

	manifest.CompletedAtMs = time.Now().UnixMilli()
	if err := s.writeCompactionManifestLocked(manifest); err != nil {
		return CompactionResult{}, err
	}
	return result, syncDirBestEffort(s.dir)
}

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

func (s *Store) writeCompactedSegmentLocked(ctx context.Context, plan *segmentCopyPlan, opts CompactionOptions) (err error) {
	logTmp := compactTempPath(s.dir, plan.newSegmentID, ".log")
	indexTmp := compactTempPath(s.dir, plan.newSegmentID, ".index")
	logFinal := segmentPath(s.dir, plan.newSegmentID, ".log")
	indexFinal := segmentPath(s.dir, plan.newSegmentID, ".index")
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

func (s *Store) compactionSafeBeforeLocked() map[string]uint64 {
	out := make(map[string]uint64)
	for streamID, state := range s.streams {
		if state.trimBefore > 0 {
			out[streamID] = state.trimBefore
		}
	}
	return out
}

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
	return reader.file.Close()
}

func (s *Store) segmentReaderBusyLocked(segmentID uint64) bool {
	reader := s.segmentReaders[segmentID]
	return reader != nil && reader.refs > 0
}

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

func compactTempPath(dir string, segmentID uint64, ext string) string {
	return filepath.Join(dir, fmt.Sprintf("segment-%08d%s.compact.tmp", segmentID, ext))
}

func maxSegmentIDOnDisk(dir string) (uint64, error) {
	segmentIDs, err := discoverSegmentIDs(dir, ".log")
	if err != nil || len(segmentIDs) == 0 {
		return 0, err
	}
	return segmentIDs[len(segmentIDs)-1], nil
}

func segmentStatByID(stats []SegmentCompactionStats, segmentID uint64) *SegmentCompactionStats {
	for i := range stats {
		if stats[i].SegmentID == segmentID {
			return &stats[i]
		}
	}
	return nil
}

func cloneSegmentCompactionStats(stats []SegmentCompactionStats) []SegmentCompactionStats {
	out := append([]SegmentCompactionStats(nil), stats...)
	for i := range out {
		out[i].Streams = append([]SegmentStreamCompactionStats(nil), out[i].Streams...)
	}
	return out
}

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

func newCompactionID() string {
	return fmt.Sprintf("compact-%d", time.Now().UnixNano())
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func syncDirBestEffort(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer file.Close()
	_ = file.Sync()
	return nil
}
