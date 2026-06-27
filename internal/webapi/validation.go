package webapi

import (
	"fmt"
	"strings"

	"github.com/logserve/logserve/gen/logservepb"
)

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

func parseSchedulingPolicy(value string) (logservepb.SchedulingPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
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

func terminalTaskStatus(status string) bool {
	return status == "SUCCEEDED" || status == "FAILED"
}

func terminalWorkflowStatus(status string) bool {
	return status == "COMPLETED" || status == "FAILED"
}
