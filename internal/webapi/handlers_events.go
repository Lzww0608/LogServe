package webapi

// This file implements polling-backed Server-Sent Events for dashboard, task,
// workflow, and raw log stream updates.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

const (
	defaultEventPollInterval = time.Second
	minEventPollInterval     = 10 * time.Millisecond
	maxEventPollInterval     = 10 * time.Second
)

// eventSubscription describes one polling-backed SSE stream: dashboard, task,
// workflow, or raw log records.
type eventSubscription struct {
	TaskID     string
	StreamID   string
	WorkflowID string
	FromSeq    uint64
	Limit      uint32
	Interval   time.Duration
}

// handleEvents implements SSE by polling the selected source and only emitting
// when the serialized payload changes.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sub, err := parseEventSubscription(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "streaming is not supported")
		return
	}
	eventName, data, err := s.nextEventPayload(r, &sub)
	if err != nil {
		writeErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := writeSSE(w, eventName, data); err != nil {
		return
	}
	flusher.Flush()
	// Keep an owned copy of the last payload because the next marshal result may be
	// reused or replaced before comparison.
	last := append([]byte(nil), data...)

	ticker := time.NewTicker(sub.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		eventName, data, err = s.nextEventPayload(r, &sub)
		if err != nil {
			errorData, _ := json.Marshal(map[string]string{"message": err.Error()})
			_ = writeSSE(w, "error", errorData)
			flusher.Flush()
			return
		}
		if bytes.Equal(data, last) {
			continue
		}
		if err := writeSSE(w, eventName, data); err != nil {
			return
		}
		flusher.Flush()
		last = append(last[:0], data...)
	}
}

// parseEventSubscription converts query parameters into a concrete SSE polling
// mode and validates mutually exclusive task/log subscriptions.
func parseEventSubscription(r *http.Request) (eventSubscription, error) {
	query := r.URL.Query()
	taskID := strings.TrimSpace(query.Get("task_id"))
	streamID := strings.TrimSpace(query.Get("stream"))
	recordsOnly := boolQuery(query.Get("records"))
	if taskID != "" && streamID != "" {
		return eventSubscription{}, fmt.Errorf("%w: use either stream or task_id, not both", errInvalidInput)
	}
	interval, err := eventPollInterval(query.Get("interval_ms"))
	if err != nil {
		return eventSubscription{}, err
	}
	sub := eventSubscription{TaskID: taskID, StreamID: streamID, FromSeq: 1, Limit: 100, Interval: interval}
	if streamID == "" {
		return sub, nil
	}
	// Workflow log streams default to workflow-summary events; records=true forces
	// raw log-record streaming for the same stream.
	if strings.HasPrefix(streamID, "wf:") && !recordsOnly {
		sub.WorkflowID = strings.TrimSpace(strings.TrimPrefix(streamID, "wf:"))
		if sub.WorkflowID == "" {
			return eventSubscription{}, fmt.Errorf("%w: workflow stream must include a workflow id", errInvalidInput)
		}
		return sub, nil
	}
	fromSeq, err := uint64Query(r, "from_seq", 1)
	if err != nil {
		return eventSubscription{}, err
	}
	limit, err := uint32Query(r, "limit", 100)
	if err != nil {
		return eventSubscription{}, err
	}
	if limit == 0 || limit > maxLogReadLimit {
		return eventSubscription{}, fmt.Errorf("%w: limit must be between 1 and %d", errInvalidInput, maxLogReadLimit)
	}
	sub.FromSeq = fromSeq
	sub.Limit = limit
	return sub, nil
}

// boolQuery parses common truthy query values for feature toggles.
func boolQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// eventPollInterval parses interval_ms and clamps it to protect the backend from
// overly aggressive polling.
func eventPollInterval(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultEventPollInterval, nil
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("%w: interval_ms must be a positive integer", errInvalidInput)
	}
	interval := time.Duration(ms) * time.Millisecond
	if interval < minEventPollInterval {
		return minEventPollInterval, nil
	}
	if interval > maxEventPollInterval {
		return maxEventPollInterval, nil
	}
	return interval, nil
}

// nextEventPayload reads the current state for one subscription and returns the
// SSE event name plus serialized JSON payload.
func (s *Server) nextEventPayload(r *http.Request, sub *eventSubscription) (string, []byte, error) {
	switch {
	case sub.TaskID != "":
		task, err := s.eventTask(r, sub.TaskID)
		if err != nil {
			return "", nil, err
		}
		data, err := json.Marshal(map[string]TaskDTO{"task": task})
		return "task", data, err
	case sub.WorkflowID != "":
		workflow, err := s.eventWorkflow(r, sub.WorkflowID)
		if err != nil {
			return "", nil, err
		}
		data, err := json.Marshal(map[string]WorkflowDTO{"workflow": workflow})
		return "workflow", data, err
	case sub.StreamID != "":
		payload, err := s.eventLogRecords(r, sub)
		if err != nil {
			return "", nil, err
		}
		data, err := json.Marshal(payload)
		return "log_records", data, err
	default:
		dashboard, err := s.dashboard(r)
		if err != nil {
			return "", nil, err
		}
		data, err := json.Marshal(map[string]DashboardDTO{"dashboard": dashboard})
		return "dashboard", data, err
	}
}

// eventTask fetches task status and merges dashboard metadata for richer SSE
// task updates.
func (s *Server) eventTask(r *http.Request, taskID string) (TaskDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: taskID})
	if err != nil {
		return TaskDTO{}, err
	}
	dto := taskStatusDTO(resp)
	if dashboard, err := s.dashboard(r); err == nil {
		dto = mergeTaskDTO(dto, dashboardTaskByID(dashboard.Tasks, taskID))
	}
	return dto, nil
}

// eventWorkflow fetches detailed workflow status for a workflow SSE update.
func (s *Server) eventWorkflow(r *http.Request, workflowID string) (WorkflowDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetWorkflowStatus(ctx, &logservepb.GetWorkflowStatusRequest{WorkflowId: workflowID})
	if err != nil {
		return WorkflowDTO{}, err
	}
	return workflowStatusDTO(resp), nil
}

// logRecordsEventPayload is the SSE payload for raw log-record subscriptions.
type logRecordsEventPayload struct {
	StreamID string         `json:"stream_id"`
	Records  []logRecordDTO `json:"records"`
	NextSeq  uint64         `json:"next_seq"`
}

// eventLogRecords reads the next log batch and advances the subscription cursor
// to avoid re-sending already delivered records.
func (s *Server) eventLogRecords(r *http.Request, sub *eventSubscription) (logRecordsEventPayload, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Log.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: sub.StreamID, FromSeq: sub.FromSeq, Limit: sub.Limit})
	if err != nil {
		return logRecordsEventPayload{}, err
	}
	records := logRecordDTOs(resp.GetRecords())
	nextSeq := sub.FromSeq
	for _, record := range records {
		if record.Seq >= nextSeq {
			nextSeq = record.Seq + 1
		}
	}
	sub.FromSeq = nextSeq
	return logRecordsEventPayload{StreamID: sub.StreamID, Records: records, NextSeq: nextSeq}, nil
}

// writeSSE writes one Server-Sent Event frame with a single JSON data line.
func writeSSE(w http.ResponseWriter, eventName string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", eventName); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n\n"))
	return err
}
