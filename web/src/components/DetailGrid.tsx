// Definition-list helper for compact detail panels.

import type { ReactNode } from "react";

// Render label/value pairs and normalize empty values to a dash.
export function DetailGrid({ items }: { items: Array<[string, ReactNode]> }) {
  return (
    <dl className="detail-grid">
      {items.map(([label, value]) => (
        <div key={label}>
          <dt>{label}</dt>
          {/* Empty values are normalized here so callers can pass raw API fields directly. */}
          <dd>{value === null || value === undefined || value === "" ? "-" : value}</dd>
        </div>
      ))}
    </dl>
  );
}
