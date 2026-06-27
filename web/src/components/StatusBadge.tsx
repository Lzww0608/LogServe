import { statusTone } from "../utils/status";

export function StatusBadge({ value }: { value?: string }) {
  const normalized = value || "UNSPECIFIED";
  return <span className={`badge ${statusTone(normalized)}`}>{normalized}</span>;
}
