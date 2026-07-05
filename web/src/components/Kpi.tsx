// KPI tile used by overview and progress panels.

import type { ReactNode } from "react";

// KpiTone matches the shared status tone vocabulary used by badges and health cards.
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
