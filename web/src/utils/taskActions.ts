// Task-action availability rules used by detail pages and regression tests.

import type { Task } from "../types/logserve";

export type TaskAction = "retry" | "resubmit" | "cancel";

export interface TaskActionState {
  enabled: boolean;
  reason?: string;
  unsupported?: boolean;
}

// Identify tasks owned by workflows, actors, or LLM flows where standalone actions are unsafe.
export function isDerivedTask(task: Task): boolean {
  return Boolean(task.workflow_id || task.step_id || task.actor_id || task.llm_model_name || task.llm_model_version);
}

// Return UI enablement and explanation for retry/resubmit/cancel task actions.
export function taskActionState(task: Task, action: TaskAction): TaskActionState {
  if (action === "cancel") {
    return {
      enabled: false,
      reason: "Cancel is not supported by this backend.",
      unsupported: true
    };
  }

  if (isDerivedTask(task)) {
    return {
      enabled: false,
      reason: "Derived tasks must be retried or resubmitted from their owning workflow, actor, or LLM surface."
    };
  }

  if (action === "retry" && task.status !== "FAILED") {
    return {
      enabled: false,
      reason: "Retry is available only for failed standalone tasks."
    };
  }

  return { enabled: true };
}