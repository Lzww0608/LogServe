package webapi

// This file normalizes backend enum values, scheduling-policy aliases, and
// terminal-state checks for polling handlers.

import (
	"fmt"
	"strings"

	"github.com/logserve/logserve/gen/logservepb"
)

// taskStatusString converts control-plane task enums into stable console labels.
func taskStatusString(status logservepb.TaskStatus) string {
	switch status {
	case logservepb.TaskStatus_TASK_STATUS_QUEUED:
		return "QUEUED"
	case logservepb.TaskStatus_TASK_STATUS_RUNNING:
		return "RUNNING"
	case logservepb.TaskStatus_TASK_STATUS_SUCCEEDED:
		return "SUCCEEDED"
	case logservepb.TaskStatus_TASK_STATUS_FAILED:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

// workflowStatusString converts workflow enums into stable console labels.
func workflowStatusString(status logservepb.WorkflowStatus) string {
	switch status {
	case logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
		return "RUNNING"
	case logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED:
		return "COMPLETED"
	case logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

// workflowStepStatusString converts workflow step enums into stable console
// labels.
func workflowStepStatusString(status logservepb.WorkflowStepStatus) string {
	switch status {
	case logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED:
		return "SCHEDULED"
	case logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_STARTED:
		return "STARTED"
	case logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED:
		return "SUCCEEDED"
	case logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

// actorStatusString converts actor enums into stable console labels.
func actorStatusString(status logservepb.ActorStatus) string {
	switch status {
	case logservepb.ActorStatus_ACTOR_STATUS_ACTIVE:
		return "ACTIVE"
	case logservepb.ActorStatus_ACTOR_STATUS_UNAVAILABLE:
		return "UNAVAILABLE"
	default:
		return "UNSPECIFIED"
	}
}

// schedulingPolicyString converts scheduler policy enums into API strings.
func schedulingPolicyString(policy logservepb.SchedulingPolicy) string {
	switch policy {
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY:
		return "RESOURCE_ONLY"
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE:
		return "LOCALITY_AWARE"
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY:
		return "PREDICTED_LATENCY"
	default:
		return "UNSPECIFIED"
	}
}

// parseSchedulingPolicy accepts the console policy aliases and returns the
// control-plane enum value.
func parseSchedulingPolicy(value string) (logservepb.SchedulingPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	// Empty input preserves the console default: locality-aware scheduling.
	case "", "locality-aware", "locality_aware", "locality aware":
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE, nil
	case "resource-only", "resource_only", "resource only":
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY, nil
	case "predicted-latency", "predicted_latency", "predicted latency":
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY, nil
	default:
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_UNSPECIFIED, fmt.Errorf("%w: unknown scheduling policy %q", errInvalidInput, value)
	}
}

// terminalTaskStatus reports whether wait loops can stop polling a task.
func terminalTaskStatus(status string) bool {
	return status == "SUCCEEDED" || status == "FAILED"
}

// terminalWorkflowStatus reports whether wait loops can stop polling a workflow.
func terminalWorkflowStatus(status string) bool {
	return status == "COMPLETED" || status == "FAILED"
}
