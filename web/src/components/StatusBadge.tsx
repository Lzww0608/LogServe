// Status badge adapter shared by task, workflow, actor, and worker tables.

import { statusTone } from "../utils/status";

// Normalize absent status values before mapping them to display tones.
export function StatusBadge({ value }: { value?: string }) {
  const normalized = value || "UNSPECIFIED";
  return <span className={`badge ${statusTone(normalized)}`}>{normalized}</span>;
}
