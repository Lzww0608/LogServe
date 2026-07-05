// Reusable presentational component for the LogServe console UI.

import { useMemo, useState } from "react";
import type { Workflow } from "../types/logserve";
import { formatTime } from "../utils/format";
import { statusTone } from "../utils/status";
import { DetailGrid } from "./DetailGrid";
import { JsonViewer } from "./JsonViewer";
import { StatusBadge } from "./StatusBadge";

// WorkflowStep reuses the API payload shape so the graph stays aligned with backend fields.
type WorkflowStep = NonNullable<Workflow["steps"]>[number];
// Position stores absolute graph coordinates in CSS pixels.
type Position = { x: number; y: number };

// DagLayout is the computed graph model shared by SVG edges and positioned nodes.
type DagLayout = {
  positions: Map<string, Position>;
  edges: Array<{ from: string; to: string }>;
  width: number;
  height: number;
};

// Layout constants keep graph geometry stable across data refreshes and node selection.
const nodeWidth = 172;
// nodeHeight matches the fixed node body used by edge y-coordinate math.
const nodeHeight = 96;
// xGap separates dependency levels so edge lines stay readable.
const xGap = 84;
// yGap separates nodes within the same dependency level.
const yGap = 34;
// margin leaves room for node focus rings and edge endpoints inside the SVG canvas.
const margin = 36;

// Render an inspectable workflow DAG with selectable nodes and a detail drawer.
export function Dag({ steps }: { steps: NonNullable<Workflow["steps"]> }) {
  // Keep selection by step id so polling updates do not reset the selected drawer when rows refresh.
  const [selectedID, setSelectedID] = useState(steps[0]?.step_id ?? "");
  const layout = useMemo(() => buildLayout(steps), [steps]);
  if (!steps.length) return <div className="empty empty-state">No steps yet. Submit or replay a workflow to populate the DAG.</div>;
  const selected = steps.find((step) => step.step_id === selectedID) ?? steps[0];
  return (
    <div className="dag-detail-layout">
      <div className="dag-graph-scroll" aria-label="Workflow DAG graph">
        <div className="dag-graph" style={{ width: layout.width, height: layout.height }}>
          <svg className="dag-graph-lines" viewBox={`0 0 ${layout.width} ${layout.height}`} aria-hidden="true">
            {layout.edges.map((edge) => {
              const from = layout.positions.get(edge.from);
              const to = layout.positions.get(edge.to);
              // Unknown dependency ids are tolerated here; validation surfaces them before submit.
              if (!from || !to) return null;
              return <line key={`${edge.from}->${edge.to}`} x1={from.x + nodeWidth} y1={from.y + nodeHeight / 2} x2={to.x} y2={to.y + nodeHeight / 2} />;
            })}
          </svg>
          {steps.map((step) => {
            const position = layout.positions.get(step.step_id) ?? { x: margin, y: margin };
            const tone = statusTone(step.status || "UNSPECIFIED");
            return (
              <button
                type="button"
                key={step.step_id}
                data-step-id={step.step_id}
                className={`dag-graph-node ${tone}${step.step_id === selected.step_id ? " selected" : ""}`}
                style={{ left: position.x, top: position.y }}
                onClick={() => setSelectedID(step.step_id)}
                aria-pressed={step.step_id === selected.step_id}
              >
                <strong>{step.step_id}</strong>
                <span>{step.task_name || "task"}</span>
                <StatusBadge value={step.status} />
                <small>{step.depends_on?.length ? `after ${step.depends_on.join(", ")}` : "root"}</small>
              </button>
            );
          })}
        </div>
      </div>
      <aside className="dag-detail-drawer" aria-label="Step detail">
        <div className="panel">
          <PanelHeading step={selected} />
          <DetailGrid items={[
            ["Step ID", selected.step_id],
            ["Task", selected.task_id || selected.task_name || "-"],
            ["Attempts", selected.attempts ?? 0],
            ["Latency", selected.latency_ms ? `${selected.latency_ms} ms` : "-"],
            ["Started", formatTime(selected.started_at_ms)],
            ["Completed", formatTime(selected.completed_at_ms)]
          ]} />
        </div>
        {selected.error && <div className="inline-error">{selected.error}</div>}
        {(selected.result_json !== undefined || selected.result_ref) && (
          <JsonViewer title="Step Result" value={selected.result_json ?? { result_ref: selected.result_ref }} collapsible />
        )}
      </aside>
    </div>
  );
}

// Render the selected DAG step heading with its status badge.
function PanelHeading({ step }: { step: WorkflowStep }) {
  return (
    <div className="panel-title">
      <h2>{step.step_id}</h2>
      <StatusBadge value={step.status} />
    </div>
  );
}

// Lay out workflow steps by dependency depth and produce SVG edge coordinates.
function buildLayout(steps: WorkflowStep[]): DagLayout {
  const byID = new Map(steps.map((step) => [step.step_id, step]));
  // Cache depths because shared dependencies can be visited by many downstream steps.
  const depthCache = new Map<string, number>();
  // Resolve dependency depth recursively; visiting guards cycles from making the render loop unbounded.
  const depthFor = (step: WorkflowStep, visiting = new Set<string>()): number => {
    if (depthCache.has(step.step_id)) return depthCache.get(step.step_id) ?? 0;
    // Cycles should already be rejected, but return root depth defensively to keep rendering finite.
    if (visiting.has(step.step_id)) return 0;
    visiting.add(step.step_id);
    const deps = step.depends_on ?? [];
    const depth = deps.length
      ? Math.max(...deps.map((dep) => {
        const depStep = byID.get(dep);
        return depStep ? depthFor(depStep, visiting) + 1 : 0;
      }))
      : 0;
    visiting.delete(step.step_id);
    depthCache.set(step.step_id, depth);
    return depth;
  };

  // Depth buckets become columns; order inside each bucket follows the backend step order.
  const levels = new Map<number, WorkflowStep[]>();
  for (const step of steps) {
    const depth = depthFor(step);
    levels.set(depth, [...(levels.get(depth) ?? []), step]);
  }

  const positions = new Map<string, Position>();
  let maxDepth = 0;
  let maxRows = 1;
  for (const [depth, levelSteps] of levels) {
    maxDepth = Math.max(maxDepth, depth);
    maxRows = Math.max(maxRows, levelSteps.length);
    levelSteps.forEach((step, index) => {
      positions.set(step.step_id, {
        x: margin + depth * (nodeWidth + xGap),
        y: margin + index * (nodeHeight + yGap)
      });
    });
  }

  // Edges preserve declared dependency direction from prerequisite step to dependent step.
  const edges = steps.flatMap((step) => (step.depends_on ?? []).map((dependency) => ({ from: dependency, to: step.step_id })));
  return {
    positions,
    edges,
    width: Math.max(640, margin * 2 + (maxDepth + 1) * nodeWidth + maxDepth * xGap),
    height: Math.max(280, margin * 2 + maxRows * nodeHeight + Math.max(0, maxRows - 1) * yGap)
  };
}
