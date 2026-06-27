import type { ReactNode } from "react";

type KpiTone = "neutral" | "good" | "bad" | "info";

export function Kpi({ label, value, tone = "neutral" }: { label: string; value: ReactNode; tone?: KpiTone }) {
  return (
    <div className={`kpi ${tone}`}>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}
