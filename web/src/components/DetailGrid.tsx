import type { ReactNode } from "react";

export function DetailGrid({ items }: { items: Array<[string, ReactNode]> }) {
  return (
    <dl className="detail-grid">
      {items.map(([label, value]) => (
        <div key={label}>
          <dt>{label}</dt>
          <dd>{value === null || value === undefined || value === "" ? "-" : value}</dd>
        </div>
      ))}
    </dl>
  );
}
