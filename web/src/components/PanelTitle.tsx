// Panel heading helper with an optional right-aligned action.

import type { ReactNode } from "react";

// Render a section heading and preserve caller-supplied action controls.
export function PanelTitle({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="panel-title">
      <h2>{title}</h2>
      {action}
    </div>
  );
}
