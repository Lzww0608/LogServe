// Read-only JSON viewer with optional collapse and copy controls.

import { useMemo, useState } from "react";
import { copyToClipboard } from "../utils/clipboard";

// Render a stable JSON/text preview for unknown API payloads.
export function JsonViewer({ value, title = "JSON", collapsible = false, defaultCollapsed = false }: { value: unknown; title?: string; collapsible?: boolean; defaultCollapsed?: boolean }) {
  const [collapsed, setCollapsed] = useState(defaultCollapsed);
  const text = useMemo(() => stringifyJSON(value), [value]);
  return (
    <div className="json-viewer">
      <div className="json-toolbar">
        <span>{title}</span>
        <div className="button-row">
          {collapsible && <button type="button" className="ghost compact-button" onClick={() => setCollapsed((current) => !current)}>{collapsed ? "Expand" : "Collapse"}</button>}
          <button type="button" className="ghost compact-button" onClick={() => void copyToClipboard(text)}>Copy</button>
        </div>
      </div>
      {!collapsed && <pre className="json">{text}</pre>}
    </div>
  );
}

// Serialize unknown values for display, falling back when JSON.stringify fails.
function stringifyJSON(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}