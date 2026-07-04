// KPI tile used by overview and progress panels.

import type { ReactNode } from "react";

type KpiTone = "neutral" | "good" | "bad" | "info" | "warn";

// Render one labeled metric with a status tone class.
export function Kpi({ label, value, tone = "neutral" }: { label: string; value: ReactNode; tone?: KpiTone }) {
  return (
    <div className={`kpi ${tone}`}>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}
