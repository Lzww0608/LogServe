import type { ReactNode } from "react";

export function PanelTitle({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="panel-title">
      <h2>{title}</h2>
      {action}
    </div>
  );
}
