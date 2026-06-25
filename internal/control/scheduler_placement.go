package control

import (
	"sort"

	"github.com/logserve/logserve/internal/metadata"
)

type placementEntry struct {
	workerID      string
	localityScore int64
	predictScore  int64
}

type placementHeap struct {
	entries []placementEntry
	byID    map[string]int
}

type modelPlacementHeaps struct {
	cached    placementHeap
	cold      placementHeap
	predicted placementPredictHeap
}

type placementPredictHeap struct {
	entries []placementEntry
	byID    map[string]int
}

type modelPlacementStore struct {
	byModel      map[modelKey]*modelPlacementHeaps
	workerCached map[string]map[modelKey]struct{}
	llmStats     map[modelKey]map[string]llmWorkerStats
}

func newModelPlacementStore() modelPlacementStore {
	return modelPlacementStore{
		byModel:      make(map[modelKey]*modelPlacementHeaps),
		workerCached: make(map[string]map[modelKey]struct{}),
		llmStats:     make(map[modelKey]map[string]llmWorkerStats),
	}
}

func (s *Scheduler) ensureModelPlacementLocked(key modelKey) *modelPlacementHeaps {
	heaps, ok := s.llmPlacement.byModel[key]
	if !ok {
		heaps = &modelPlacementHeaps{}
		s.llmPlacement.byModel[key] = heaps
		for workerID, view := range s.workerViews {
			s.upsertWorkerModelPlacementInnerLocked(key, workerID, view, heaps)
		}
	}
	return heaps
}

func (s *Scheduler) WorkerCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workerViews)
}

func (s *Scheduler) SyncLLMStats(stats map[llmStatsKey]llmWorkerStats) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLLMStatsLocked(stats)
}

func (s *Scheduler) UpdateLLMStats(modelName, modelVersion, workerID string, stats llmWorkerStats) {
	if s == nil || workerID == "" || modelName == "" {
		return
	}
	key := modelKeyFromParts(modelName, modelVersion)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.llmPlacement.llmStats[key] == nil {
		s.llmPlacement.llmStats[key] = make(map[string]llmWorkerStats)
	}
	s.llmPlacement.llmStats[key][workerID] = stats
	view, ok := s.workerViews[workerID]
	if !ok {
		return
	}
	s.upsertWorkerModelPlacementLocked(key, workerID, view)
}

func (s *Scheduler) refreshWorkerPlacementLocked(workerID string, view workerView, previous workerView) {
	if s == nil {
		return
	}
	affected := make(map[modelKey]struct{})
	for key := range previous.cachedModels {
		affected[key] = struct{}{}
	}
	for key := range view.cachedModels {
		affected[key] = struct{}{}
	}
	for key := range s.llmPlacement.byModel {
		affected[key] = struct{}{}
	}
	for key := range affected {
		s.upsertWorkerModelPlacementLocked(key, workerID, view)
	}
}

func (s *Scheduler) removeWorkerPlacementIndexLocked(workerID string, view workerView) {
	delete(s.llmPlacement.workerCached, workerID)
	for key := range s.llmPlacement.byModel {
		heaps := s.llmPlacement.byModel[key]
		heaps.cached.remove(workerID)
		heaps.cold.remove(workerID)
		heaps.predicted.remove(workerID)
	}
}

func (s *Scheduler) upsertWorkerModelPlacementLocked(key modelKey, workerID string, view workerView) {
	heaps := s.ensureModelPlacementLocked(key)
	s.upsertWorkerModelPlacementInnerLocked(key, workerID, view, heaps)
}

func (s *Scheduler) upsertWorkerModelPlacementInnerLocked(key modelKey, workerID string, view workerView, heaps *modelPlacementHeaps) {
	modelKeyStr := metadata.ModelKey(key.name, key.version)
	_, cacheHit := view.cachedModels[key]
	localityScore := localityBaseScore(view, cacheHit)
	stats := s.placementStatsLocked(key, workerID)
	predictScore := predictedPlacementScore(view, modelKeyStr, stats, cacheHit)

	if !workerPlacementActive(view) {
		heaps.cached.remove(workerID)
		heaps.cold.remove(workerID)
		heaps.predicted.remove(workerID)
		if cached := s.llmPlacement.workerCached[workerID]; cached != nil {
			delete(cached, key)
		}
		return
	}

	if cacheHit {
		heaps.cached.upsert(workerID, localityScore, predictScore)
		heaps.cold.remove(workerID)
		if s.llmPlacement.workerCached[workerID] == nil {
			s.llmPlacement.workerCached[workerID] = make(map[modelKey]struct{})
		}
		s.llmPlacement.workerCached[workerID][key] = struct{}{}
	} else {
		heaps.cold.upsert(workerID, localityScore, predictScore)
		heaps.cached.remove(workerID)
		if cached := s.llmPlacement.workerCached[workerID]; cached != nil {
			delete(cached, key)
		}
	}
	heaps.predicted.upsert(workerID, predictScore)
}

func (h *placementPredictHeap) upsert(workerID string, predictScore int64) {
	if h.byID == nil {
		h.byID = make(map[string]int)
	}
	if predictScore >= 1<<61 {
		h.remove(workerID)
		return
	}
	entry := placementEntry{workerID: workerID, predictScore: predictScore}
	if idx, ok := h.byID[workerID]; ok && idx < len(h.entries) && h.entries[idx].workerID == workerID {
		h.entries[idx] = entry
	} else {
		h.entries = append(h.entries, entry)
		idx = len(h.entries) - 1
		h.byID[workerID] = idx
	}
	sort.Slice(h.entries, func(i, j int) bool {
		if h.entries[i].predictScore == h.entries[j].predictScore {
			return h.entries[i].workerID < h.entries[j].workerID
		}
		return h.entries[i].predictScore < h.entries[j].predictScore
	})
	h.byID = make(map[string]int, len(h.entries))
	for i, item := range h.entries {
		h.byID[item.workerID] = i
	}
}

func (h *placementPredictHeap) remove(workerID string) {
	if h.byID == nil {
		return
	}
	idx, ok := h.byID[workerID]
	if !ok || idx >= len(h.entries) || h.entries[idx].workerID != workerID {
		delete(h.byID, workerID)
		return
	}
	h.entries = append(h.entries[:idx], h.entries[idx+1:]...)
	h.byID = make(map[string]int, len(h.entries))
	for i, item := range h.entries {
		h.byID[item.workerID] = i
	}
}

func (s *Scheduler) placementStatsLocked(key modelKey, workerID string) llmWorkerStats {
	if workers, ok := s.llmPlacement.llmStats[key]; ok {
		if stats, ok := workers[workerID]; ok {
			return stats
		}
	}
	return llmWorkerStats{}
}

func workerPlacementActive(view workerView) bool {
	return view.workerID != ""
}

func localityBaseScore(view workerView, cacheHit bool) int64 {
	if !view.hasCapacity() {
		return -(1 << 60)
	}
	available := int(view.capacity - view.runningTasks)
	score := int64(available*100 - int(view.runningTasks)*10)
	if cacheHit {
		score += 1000
	}
	return score
}

func predictedPlacementScore(view workerView, modelKey string, stats llmWorkerStats, cacheHit bool) int64 {
	if !view.hasCapacity() {
		return 1 << 62
	}
	worker := metadata.Worker{
		WorkerID:     view.workerID,
		CachedModels: map[string]bool{modelKey: cacheHit},
		Capacity:     view.capacity,
		RunningTasks: view.runningTasks,
	}
	return predictedLatencyMs(worker, modelKey, stats)
}

func (s *Scheduler) PreferredPredictedWorker(key modelKey, modelKeyStr string, queueDelayMs, waitMs int64) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	heaps := s.ensureModelPlacementLocked(key)
	bestWorker := ""
	bestPrediction := int64(1<<62 - 1)
	for _, entry := range heaps.predicted.entries {
		view, ok := s.workerViews[entry.workerID]
		if !ok || !view.hasCapacity() {
			continue
		}
		predicted := entry.predictScore + int64(view.runningTasks)*50
		_, cacheHit := view.cachedModels[key]
		if queueDelayMs > waitMs && !cacheHit {
			predicted -= 25
		}
		if bestWorker == "" || predicted < bestPrediction || (predicted == bestPrediction && entry.workerID < bestWorker) {
			bestWorker = entry.workerID
			bestPrediction = predicted
		}
	}
	return bestWorker
}

func (s *Scheduler) preferredLocalityFromPlacementLocked(key modelKey, queueDelayMs, waitMs int64) string {
	heaps := s.ensureModelPlacementLocked(key)
	hasCachedAvailable := heaps.cached.hasCapacityLocked(s.workerViews)

	if workerID := heaps.cached.bestWorkerID(s.workerViews); workerID != "" {
		return workerID
	}

	bestWorker := ""
	bestScore := int64(-1 << 60)
	for _, entry := range heaps.cold.entries {
		view, ok := s.workerViews[entry.workerID]
		if !ok || !view.hasCapacity() {
			continue
		}
		score := entry.localityScore
		if hasCachedAvailable && queueDelayMs < waitMs {
			score -= 1000
		}
		if bestWorker == "" || score > bestScore || (score == bestScore && entry.workerID < bestWorker) {
			bestWorker = entry.workerID
			bestScore = score
		}
	}
	return bestWorker
}

func (h *placementHeap) upsert(workerID string, localityScore, predictScore int64) {
	if h.byID == nil {
		h.byID = make(map[string]int)
	}
	if localityScore <= -(1 << 59) {
		h.remove(workerID)
		return
	}
	entry := placementEntry{
		workerID:      workerID,
		localityScore: localityScore,
		predictScore:  predictScore,
	}
	if idx, ok := h.byID[workerID]; ok && idx < len(h.entries) && h.entries[idx].workerID == workerID {
		h.entries[idx] = entry
	} else {
		h.entries = append(h.entries, entry)
	}
	sort.Slice(h.entries, func(i, j int) bool {
		if h.entries[i].localityScore == h.entries[j].localityScore {
			return h.entries[i].workerID < h.entries[j].workerID
		}
		return h.entries[i].localityScore > h.entries[j].localityScore
	})
	h.byID = make(map[string]int, len(h.entries))
	for i, item := range h.entries {
		h.byID[item.workerID] = i
	}
}

func (h *placementHeap) remove(workerID string) {
	if h.byID == nil {
		return
	}
	idx, ok := h.byID[workerID]
	if !ok || idx >= len(h.entries) || h.entries[idx].workerID != workerID {
		delete(h.byID, workerID)
		return
	}
	h.entries = append(h.entries[:idx], h.entries[idx+1:]...)
	h.byID = make(map[string]int, len(h.entries))
	for i, item := range h.entries {
		h.byID[item.workerID] = i
	}
}

func placementBetterLocality(a, b placementEntry) bool {
	if a.localityScore == b.localityScore {
		return a.workerID < b.workerID
	}
	return a.localityScore > b.localityScore
}

func (h *placementHeap) hasCapacityLocked(views map[string]workerView) bool {
	for _, entry := range h.entries {
		view, ok := views[entry.workerID]
		if ok && view.hasCapacity() {
			return true
		}
	}
	return false
}

func (h *placementHeap) bestWorkerID(views map[string]workerView) string {
	for _, entry := range h.entries {
		view, ok := views[entry.workerID]
		if ok && view.hasCapacity() {
			return entry.workerID
		}
	}
	return ""
}

func (s *Scheduler) syncLLMStatsLocked(stats map[llmStatsKey]llmWorkerStats) {
	for key, item := range stats {
		modelKey := modelKeyFromParts(key.modelName, key.modelVersion)
		if s.llmPlacement.llmStats[modelKey] == nil {
			s.llmPlacement.llmStats[modelKey] = make(map[string]llmWorkerStats)
		}
		s.llmPlacement.llmStats[modelKey][key.workerID] = item
		view, ok := s.workerViews[key.workerID]
		if !ok {
			continue
		}
		s.upsertWorkerModelPlacementLocked(modelKey, key.workerID, view)
	}
}

func sortedPlacementWorkerIDs(views map[string]workerView) []string {
	out := make([]string, 0, len(views))
	for workerID := range views {
		out = append(out, workerID)
	}
	sort.Strings(out)
	return out
}
