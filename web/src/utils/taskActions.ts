// Task-action availability rules used by detail pages and regression tests.

import type { Task } from "../types/logserve";

// TaskAction names mirror the backend task operation route suffixes.
export type TaskAction = "retry" | "resubmit" | "cancel";

// TaskActionState carries both button enablement and the explanation shown to operators.
export interface TaskActionState {
  enabled: boolean;
  reason?: string;
  unsupported?: boolean;
}

// Identify tasks owned by workflows, actors, or LLM flows where standalone actions are unsafe.
export function isDerivedTask(task: Task): boolean {
  // Ownership metadata means another surface owns retry semantics and state consistency.
  return Boolean(task.workflow_id || task.step_id || task.actor_id || task.llm_model_name || task.llm_model_version);
}

// Return UI enablement and explanation for retry/resubmit/cancel task actions.
export function taskActionState(task: Task, action: TaskAction): TaskActionState {
  // Keep cancel visible but disabled so users understand the backend capability is absent.
  if (action === "cancel") {
    return {
      enabled: false,
      reason: "Cancel is not supported by this backend.",
      unsupported: true
    };
  }

  // Derived work can carry workflow/actor/LLM invariants that standalone resubmission would bypass.
  if (isDerivedTask(task)) {
    return {
      enabled: false,
      reason: "Derived tasks must be retried or resubmitted from their owning workflow, actor, or LLM surface."
    };
  }

  // Retrying non-failed standalone tasks would duplicate work the backend still considers live or complete.
  if (action === "retry" && task.status !== "FAILED") {
    return {
      enabled: false,
      reason: "Retry is available only for failed standalone tasks."
    };
  }

  // At this point retry/resubmit is for standalone work and any remaining backend validation happens server-side.
  return { enabled: true };
}