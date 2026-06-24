package control

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/logrecord"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/workflow"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	metadataCheckpointStream = "system:checkpoints"
	metadataCheckpointEvent  = "MetadataCheckpointCreated"
)

type MetadataCheckpoint struct {
	ID          string                              `json:"checkpoint_id"`
	CreatedAtMs int64                               `json:"created_at_ms"`
	Streams     map[string]MetadataCheckpointStream `json:"streams"`
	Tasks       []metadataCheckpointTask            `json:"tasks,omitempty"`
	Workflows   []metadataCheckpointWorkflow        `json:"workflows,omitempty"`
	Actors      []metadataCheckpointActor           `json:"actors,omitempty"`
	LLMStats    []metadataCheckpointLLMStats        `json:"llm_stats,omitempty"`
}

type MetadataCheckpointStream struct {
	Kind    string `json:"kind,omitempty"`
	LastSeq uint64 `json:"last_seq"`
}

type metadataCheckpointTask struct {
	Task metadata.Task   `json:"task"`
	Spec json.RawMessage `json:"spec"`
}

type metadataCheckpointWorkflow struct {
	WorkflowID string         `json:"workflow_id"`
	State      workflow.State `json:"state"`
}

type metadataCheckpointActor struct {
	ActorID string      `json:"actor_id"`
	State   actor.State `json:"state"`
}

type metadataCheckpointLLMStats struct {
	ModelName    string         `json:"model_name"`
	ModelVersion string         `json:"model_version"`
	WorkerID     string         `json:"worker_id"`
	Stats        llmWorkerStats `json:"stats"`
}

type MetadataCheckpointConsistency struct {
	Consistent    bool     `json:"consistent"`
	CheckedCount  int      `json:"checked_count"`
	FailureKeys   []string `json:"failure_keys,omitempty"`
	CheckpointID  string   `json:"checkpoint_id,omitempty"`
	CheckpointAge int64    `json:"checkpoint_age_ms,omitempty"`
}

func (s *Service) CreateMetadataCheckpoint(ctx context.Context, retention int) (MetadataCheckpoint, error) {
	now := time.Now().UnixMilli()
	cp := MetadataCheckpoint{
		ID:          "checkpoint-" + randomHex(),
		CreatedAtMs: now,
		Streams:     make(map[string]MetadataCheckpointStream),
	}
	lastSeqs, err := s.streamLastSeqs(ctx, []string{"task:", "wf:", "actor:", "llm:"})
	if err != nil {
		return MetadataCheckpoint{}, err
	}
	snapshotTasks(&cp, s, lastSeqs)
	snapshotWorkflows(&cp, s, lastSeqs)
	snapshotActors(&cp, s, lastSeqs)
	snapshotLLMStats(&cp, s, lastSeqs)

	payload, err := json.Marshal(cp)
	if err != nil {
		return MetadataCheckpoint{}, err
	}
	resp, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       metadataCheckpointStream,
		EventType:      metadataCheckpointEvent,
		IdempotencyKey: cp.ID,
		Payload:        payload,
	})
	if err != nil {
		return MetadataCheckpoint{}, err
	}
	if retention > 0 && resp.GetSeq() > uint64(retention) {
		beforeSeq := resp.GetSeq() - uint64(retention) + 1
		if _, err := s.log.TrimStream(ctx, &logservepb.TrimStreamRequest{StreamId: metadataCheckpointStream, BeforeSeq: beforeSeq}); err != nil {
			observability.Error("metadata_checkpoint_retention_failed", err, map[string]any{"before_seq": beforeSeq})
		}
	}
	return cp, nil
}

func (s *Service) StartMetadataCheckpointLoop(ctx context.Context, interval time.Duration, retention int) func() {
	if interval <= 0 {
		return func() {}
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.CreateMetadataCheckpoint(loopCtx, retention); err != nil {
					observability.Error("metadata_checkpoint_create_failed", err, nil)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
func (s *Service) CheckMetadataCheckpointConsistency(ctx context.Context) (MetadataCheckpointConsistency, error) {
	cp, err := s.loadLatestMetadataCheckpoint(ctx)
	if err != nil {
		return MetadataCheckpointConsistency{}, err
	}
	if cp == nil {
		return MetadataCheckpointConsistency{Consistent: true}, nil
	}
	checkpoint := *cp
	checkpoint.normalizeStreamKinds()

	fullMeta := metadata.NewMemoryStore()
	full := NewServiceWithResultStore(fullMeta, s.log, s.resultStore, s.resultInlineThreshold)
	if err := full.bootstrapTasks(ctx); err != nil {
		return MetadataCheckpointConsistency{}, err
	}
	if err := full.bootstrapWorkflowsWithScheduling(ctx, false); err != nil {
		return MetadataCheckpointConsistency{}, err
	}
	if err := full.bootstrapActors(ctx); err != nil {
		return MetadataCheckpointConsistency{}, err
	}

	fastMeta := metadata.NewMemoryStore()
	fast := NewServiceWithResultStore(fastMeta, s.log, s.resultStore, s.resultInlineThreshold)
	if err := fast.bootstrapMetadataFromCheckpointWithScheduling(ctx, checkpoint, false); err != nil {
		return MetadataCheckpointConsistency{}, err
	}
	expectedLLMStats, err := s.llmStatsFromCheckpointTail(ctx, checkpoint)
	if err != nil {
		return MetadataCheckpointConsistency{}, err
	}

	result := MetadataCheckpointConsistency{
		Consistent:    true,
		CheckpointID:  checkpoint.ID,
		CheckpointAge: time.Now().UnixMilli() - checkpoint.CreatedAtMs,
	}
	for _, task := range fullMeta.ListTasks() {
		result.CheckedCount++
		other, ok := fastMeta.GetTask(task.TaskID)
		if !ok || !metadataTasksConsistent(task, other) {
			result.Consistent = false
			result.FailureKeys = append(result.FailureKeys, "task:"+task.TaskID)
		}
	}
	for _, state := range fullMeta.ListWorkflows() {
		result.CheckedCount++
		other, ok := fastMeta.GetWorkflow(state.WorkflowID)
		if !ok || !workflow.Consistent(state, other) {
			result.Consistent = false
			result.FailureKeys = append(result.FailureKeys, "wf:"+state.WorkflowID)
		}
	}
	for _, state := range fullMeta.ListActors() {
		result.CheckedCount++
		other, ok := fastMeta.GetActor(state.ActorID)
		if !ok || !actor.Consistent(state, other) {
			result.Consistent = false
			result.FailureKeys = append(result.FailureKeys, "actor:"+state.ActorID)
		}
	}
	if !llmStatsMapsConsistent(expectedLLMStats, fast.llmStatsSnapshot()) {
		result.CheckedCount++
		result.Consistent = false
		result.FailureKeys = append(result.FailureKeys, "llm_stats")
	}
	sort.Strings(result.FailureKeys)
	return result, nil
}

func (s *Service) loadLatestMetadataCheckpoint(ctx context.Context) (*MetadataCheckpoint, error) {
	var latest *MetadataCheckpoint
	if err := s.forEachRawLogRecord(ctx, metadataCheckpointStream, 1, func(rec logrecord.RawRecord) error {
		if rec.EventType != metadataCheckpointEvent {
			return nil
		}
		var cp MetadataCheckpoint
		if err := json.Unmarshal(rec.Payload, &cp); err != nil {
			observability.Error("metadata_checkpoint_decode_failed", err, map[string]any{"seq": rec.Seq})
			return nil
		}
		if cp.Streams == nil {
			cp.Streams = make(map[string]MetadataCheckpointStream)
		}
		if latest == nil || cp.CreatedAtMs >= latest.CreatedAtMs {
			clone := cp
			latest = &clone
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return latest, nil
}

func (s *Service) bootstrapMetadataFromCheckpoint(ctx context.Context, cp MetadataCheckpoint) error {
	return s.bootstrapMetadataFromCheckpointWithScheduling(ctx, cp, true)
}

func (s *Service) bootstrapMetadataFromCheckpointWithScheduling(ctx context.Context, cp MetadataCheckpoint, schedule bool) error {
	if err := s.bootstrapCheckpointTasks(ctx, cp); err != nil {
		return err
	}
	if err := s.bootstrapCheckpointWorkflows(ctx, cp, schedule); err != nil {
		return err
	}
	if err := s.bootstrapCheckpointActors(ctx, cp); err != nil {
		return err
	}
	if err := s.bootstrapCheckpointLLMStats(ctx, cp); err != nil {
		return err
	}
	return nil
}

func (s *Service) bootstrapCheckpointTasks(ctx context.Context, cp MetadataCheckpoint) error {
	seen := make(map[string]struct{}, len(cp.Tasks))
	for _, item := range cp.Tasks {
		spec := &logservepb.TaskSpec{}
		if len(item.Spec) > 0 {
			if err := protojson.Unmarshal(item.Spec, spec); err != nil {
				return err
			}
		}
		state := taskReplayStateFromCheckpoint(item.Task, spec)
		streamID := taskStream(item.Task.TaskID)
		seen[streamID] = struct{}{}
		fromSeq := cp.Streams[streamID].LastSeq + 1
		if fromSeq == 1 {
			fromSeq = 2
		}
		if err := s.forEachRawLogRecord(ctx, streamID, fromSeq, func(rec logrecord.RawRecord) error {
			return state.applyRaw(rec)
		}); err != nil {
			return err
		}
		if err := s.restoreTaskReplayState(state); err != nil {
			return err
		}
	}
	streams, err := s.listStreams(ctx, "task:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		if _, ok := seen[streamID]; ok {
			continue
		}
		state, err := replayTaskMetadataRawEach(func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, 1, emit)
		}, nil)
		if err != nil {
			return err
		}
		if err := s.restoreTaskReplayState(state); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) bootstrapCheckpointWorkflows(ctx context.Context, cp MetadataCheckpoint, schedule bool) error {
	seen := make(map[string]struct{}, len(cp.Workflows))
	for _, item := range cp.Workflows {
		streamID := workflowStream(item.WorkflowID)
		seen[streamID] = struct{}{}
		state := item.State
		fromSeq := cp.Streams[streamID].LastSeq + 1
		if fromSeq == 1 {
			fromSeq = 2
		}
		tail, err := workflow.ReplayFromRawEach(state, func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, fromSeq, emit)
		})
		if err != nil {
			return err
		}
		s.prepareRetryableFailedSteps(&tail)
		s.meta.UpsertWorkflow(tail)
		if !schedule {
			continue
		}
		if err := s.restoreWorkflowTasks(tail); err != nil {
			return err
		}
		if tail.Status == logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
			if err := s.scheduleReadySteps(ctx, tail.WorkflowID); err != nil {
				return err
			}
		}
	}
	streams, err := s.listStreams(ctx, "wf:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		if _, ok := seen[streamID]; ok {
			continue
		}
		workflowID := strings.TrimPrefix(streamID, "wf:")
		state, err := workflow.ReplayRawEach(workflowID, func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, 1, emit)
		})
		if err != nil {
			continue
		}
		s.prepareRetryableFailedSteps(&state)
		s.meta.UpsertWorkflow(state)
		if !schedule {
			continue
		}
		if err := s.restoreWorkflowTasks(state); err != nil {
			return err
		}
		if state.Status == logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
			if err := s.scheduleReadySteps(ctx, state.WorkflowID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) bootstrapCheckpointActors(ctx context.Context, cp MetadataCheckpoint) error {
	seen := make(map[string]struct{}, len(cp.Actors))
	for _, item := range cp.Actors {
		streamID := actorStream(item.ActorID)
		seen[streamID] = struct{}{}
		fromSeq := cp.Streams[streamID].LastSeq + 1
		if fromSeq == 1 {
			fromSeq = 2
		}
		replayed, err := actor.ReplayFromStateRawEach(item.ActorID, item.State, func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, fromSeq, emit)
		}, s)
		if err != nil {
			return err
		}
		s.meta.UpsertActor(replayed.State)
	}
	streams, err := s.listStreams(ctx, "actor:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		if _, ok := seen[streamID]; ok {
			continue
		}
		actorID := strings.TrimPrefix(streamID, "actor:")
		replayed, err := actor.ReplayRawEach(actorID, func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, 1, emit)
		}, s)
		if err != nil {
			continue
		}
		s.meta.UpsertActor(replayed.State)
	}
	return nil
}

func (s *Service) bootstrapCheckpointLLMStats(ctx context.Context, cp MetadataCheckpoint) error {
	s.restoreCheckpointLLMStats(cp)
	return s.materializeLLMTailsFromCheckpoint(ctx, cp)
}

func (s *Service) restoreCheckpointLLMStats(cp MetadataCheckpoint) {
	s.llmStatsMu.Lock()
	s.llmStats = llmStatsFromCheckpoint(cp)
	s.llmStatsMu.Unlock()
}

func llmStatsFromCheckpoint(cp MetadataCheckpoint) map[llmStatsKey]llmWorkerStats {
	stats := make(map[llmStatsKey]llmWorkerStats, len(cp.LLMStats))
	for _, item := range cp.LLMStats {
		version := firstNonEmpty(item.ModelVersion, "v1")
		stats[llmStatsKey{modelName: item.ModelName, modelVersion: version, workerID: item.WorkerID}] = item.Stats
	}
	return stats
}

func (s *Service) materializeLLMTailsFromCheckpoint(ctx context.Context, cp MetadataCheckpoint) error {
	seen := make(map[string]struct{})
	for _, streamID := range checkpointLLMStreams(cp) {
		entry := cp.Streams[streamID]
		seen[streamID] = struct{}{}
		if err := s.materializeLLMStreamFromSeq(ctx, streamID, entry.LastSeq+1); err != nil {
			return err
		}
	}
	streams, err := s.listStreams(ctx, "llm:")
	if err != nil {
		return err
	}
	sort.Strings(streams)
	for _, streamID := range streams {
		if _, ok := seen[streamID]; ok {
			continue
		}
		if err := s.materializeLLMStreamFromSeq(ctx, streamID, 1); err != nil {
			return err
		}
	}
	return nil
}

func checkpointLLMStreams(cp MetadataCheckpoint) []string {
	streams := make([]string, 0, len(cp.Streams))
	for streamID, entry := range cp.Streams {
		if !strings.HasPrefix(streamID, "llm:") {
			continue
		}
		if entry.Kind != "" && entry.Kind != "llm" {
			continue
		}
		streams = append(streams, streamID)
	}
	sort.Strings(streams)
	return streams
}

func (s *Service) llmStatsFromCheckpointTail(ctx context.Context, cp MetadataCheckpoint) (map[llmStatsKey]llmWorkerStats, error) {
	verifier := NewServiceWithResultStore(metadata.NewMemoryStore(), s.log, s.resultStore, s.resultInlineThreshold)
	verifier.restoreCheckpointLLMStats(cp)
	if err := verifier.materializeLLMTailsFromCheckpoint(ctx, cp); err != nil {
		return nil, err
	}
	return verifier.llmStatsSnapshot(), nil
}

func (s *Service) materializeLLMStreamFromSeq(ctx context.Context, streamID string, fromSeq uint64) error {
	return s.forEachRawLogRecord(ctx, streamID, fromSeq, func(rec logrecord.RawRecord) error {
		if rec.EventType != "LLMCompleted" {
			return nil
		}
		var payload llmEventPayload
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return err
		}
		if payload.TimestampMs == 0 {
			payload.TimestampMs = rec.TimestampMs
		}
		s.materializeLLMCompleted(payload)
		return nil
	})
}

func (s *Service) forEachLogRecordFromSeq(ctx context.Context, streamID string, fromSeq uint64, emit func(*logservepb.LogRecord) error) error {
	if emit == nil {
		return nil
	}
	if fromSeq == 0 {
		fromSeq = 1
	}
	for {
		resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
			StreamId: streamID,
			FromSeq:  fromSeq,
			Limit:    bootstrapReadLimit,
		})
		if err != nil {
			return err
		}
		records := resp.GetRecords()
		if len(records) == 0 {
			return nil
		}
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		fromSeq = records[len(records)-1].GetSeq() + 1
		if len(records) < bootstrapReadLimit {
			return nil
		}
	}
}

func snapshotTasks(cp *MetadataCheckpoint, s *Service, lastSeqs map[string]uint64) {
	tasks := s.meta.ListTasks()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })
	for _, task := range tasks {
		spec := s.specForTask(task.TaskID)
		if spec == nil {
			continue
		}
		specJSON, err := protojson.Marshal(cloneSpec(spec))
		if err != nil {
			continue
		}
		cp.Tasks = append(cp.Tasks, metadataCheckpointTask{Task: task, Spec: specJSON})
		cp.Streams[taskStream(task.TaskID)] = MetadataCheckpointStream{Kind: "task", LastSeq: lastSeqs[taskStream(task.TaskID)]}
	}
}

func snapshotWorkflows(cp *MetadataCheckpoint, s *Service, lastSeqs map[string]uint64) {
	states := s.meta.ListWorkflows()
	sort.Slice(states, func(i, j int) bool { return states[i].WorkflowID < states[j].WorkflowID })
	for _, state := range states {
		cp.Workflows = append(cp.Workflows, metadataCheckpointWorkflow{WorkflowID: state.WorkflowID, State: state})
		cp.Streams[workflowStream(state.WorkflowID)] = MetadataCheckpointStream{Kind: "workflow", LastSeq: lastSeqs[workflowStream(state.WorkflowID)]}
	}
}

func snapshotActors(cp *MetadataCheckpoint, s *Service, lastSeqs map[string]uint64) {
	states := s.meta.ListActors()
	sort.Slice(states, func(i, j int) bool { return states[i].ActorID < states[j].ActorID })
	for _, state := range states {
		cp.Actors = append(cp.Actors, metadataCheckpointActor{ActorID: state.ActorID, State: state})
		cp.Streams[actorStream(state.ActorID)] = MetadataCheckpointStream{Kind: "actor", LastSeq: lastSeqs[actorStream(state.ActorID)]}
	}
}

func snapshotLLMStats(cp *MetadataCheckpoint, s *Service, lastSeqs map[string]uint64) {
	s.llmStatsMu.RLock()
	keys := make([]llmStatsKey, 0, len(s.llmStats))
	for key := range s.llmStats {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].modelName != keys[j].modelName {
			return keys[i].modelName < keys[j].modelName
		}
		if keys[i].modelVersion != keys[j].modelVersion {
			return keys[i].modelVersion < keys[j].modelVersion
		}
		return keys[i].workerID < keys[j].workerID
	})
	for _, key := range keys {
		cp.LLMStats = append(cp.LLMStats, metadataCheckpointLLMStats{
			ModelName:    key.modelName,
			ModelVersion: key.modelVersion,
			WorkerID:     key.workerID,
			Stats:        s.llmStats[key],
		})
	}
	s.llmStatsMu.RUnlock()
	for streamID, lastSeq := range lastSeqs {
		if strings.HasPrefix(streamID, "llm:") {
			cp.Streams[streamID] = MetadataCheckpointStream{Kind: "llm", LastSeq: lastSeq}
		}
	}
}

func (s *Service) streamLastSeqs(ctx context.Context, prefixes []string) (map[string]uint64, error) {
	out := make(map[string]uint64)
	for _, prefix := range prefixes {
		resp, err := s.log.GetStreamStats(ctx, &logservepb.GetStreamStatsRequest{Prefix: prefix})
		if err != nil {
			return nil, err
		}
		for _, stats := range resp.GetStreams() {
			if stats.GetNextSeq() > 1 {
				out[stats.GetStreamId()] = stats.GetNextSeq() - 1
			}
		}
		if len(resp.GetStreams()) > 0 {
			continue
		}
		streams, err := s.listStreams(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, streamID := range streams {
			lastSeq, err := s.streamLastSeqByRead(ctx, streamID)
			if err != nil {
				return nil, err
			}
			out[streamID] = lastSeq
		}
	}
	return out, nil
}

func (s *Service) streamLastSeqByRead(ctx context.Context, streamID string) (uint64, error) {
	var last uint64
	err := s.forEachRawLogRecord(ctx, streamID, 1, func(rec logrecord.RawRecord) error {
		if rec.Seq > last {
			last = rec.Seq
		}
		return nil
	})
	return last, err
}

func (s *Service) restoreTaskReplayState(state *taskReplayState) error {
	if state == nil || !state.ok || state.spec == nil {
		return nil
	}
	task := state.finalTask()
	created, _ := s.meta.CreateTask(task, task.IdempotencyKey)
	s.specMu.Lock()
	s.specs[task.TaskID] = cloneSpec(state.spec)
	s.specMu.Unlock()
	if created.Status == logservepb.TaskStatus_TASK_STATUS_QUEUED || created.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		if s.useSchedulerV2() {
			s.scheduler.Enqueue(s.schedulerMetaFromTask(created))
		} else {
			s.queueMu.Lock()
			if !containsTaskID(s.queue, created.TaskID) {
				s.queue = append(s.queue, created.TaskID)
			}
			s.queueMu.Unlock()
		}
	}
	return nil
}

func (s *Service) llmStatsSnapshot() map[llmStatsKey]llmWorkerStats {
	s.llmStatsMu.RLock()
	defer s.llmStatsMu.RUnlock()
	out := make(map[llmStatsKey]llmWorkerStats, len(s.llmStats))
	for key, stats := range s.llmStats {
		out[key] = stats
	}
	return out
}

func llmStatsMapsConsistent(a, b map[llmStatsKey]llmWorkerStats) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if bv, ok := b[key]; !ok || av != bv {
			return false
		}
	}
	return true
}

func metadataTasksConsistent(a, b metadata.Task) bool {
	return a.TaskID == b.TaskID &&
		a.TaskName == b.TaskName &&
		a.Status == b.Status &&
		a.Error == b.Error &&
		a.WorkerID == b.WorkerID &&
		a.WorkflowID == b.WorkflowID &&
		a.StepID == b.StepID &&
		a.TargetWorkerID == b.TargetWorkerID &&
		a.ActorID == b.ActorID &&
		a.ActorCallID == b.ActorCallID &&
		a.ActorEpoch == b.ActorEpoch &&
		a.ActorCommandSeq == b.ActorCommandSeq &&
		a.TaskLeaseEpoch == b.TaskLeaseEpoch &&
		a.LLMModelName == b.LLMModelName &&
		a.LLMModelVersion == b.LLMModelVersion &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.IdempotencyFingerprint == b.IdempotencyFingerprint &&
		string(a.ResultJSON) == string(b.ResultJSON)
}

func checkpointStreamKind(streamID string) string {
	switch {
	case strings.HasPrefix(streamID, "task:"):
		return "task"
	case strings.HasPrefix(streamID, "wf:"):
		return "workflow"
	case strings.HasPrefix(streamID, "actor:"):
		return "actor"
	case strings.HasPrefix(streamID, "llm:"):
		return "llm"
	default:
		return ""
	}
}

func (cp *MetadataCheckpoint) normalizeStreamKinds() {
	for streamID, entry := range cp.Streams {
		if entry.Kind == "" {
			entry.Kind = checkpointStreamKind(streamID)
			cp.Streams[streamID] = entry
		}
	}
}

func (cp MetadataCheckpoint) String() string {
	return fmt.Sprintf("%s streams=%d tasks=%d workflows=%d actors=%d llm_stats=%d", cp.ID, len(cp.Streams), len(cp.Tasks), len(cp.Workflows), len(cp.Actors), len(cp.LLMStats))
}
