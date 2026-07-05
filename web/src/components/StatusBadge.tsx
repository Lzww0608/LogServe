// Status badge adapter shared by task, workflow, actor, and worker tables.

import { statusTone } from "../utils/status";

// Normalize absent status values before mapping them to display tones.
export function StatusBadge({ value }: { value?: string }) {
  // Empty backend status fields render as explicit UNSPECIFIED instead of a blank badge.
  const normalized = value || "UNSPECIFIED";
  return <span className={`badge ${statusTone(normalized)}`}>{normalized}</span>;
}
