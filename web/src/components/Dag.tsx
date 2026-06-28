import { useMemo, useState } from "react";
import type { Workflow } from "../types/logserve";
import { formatTime } from "../utils/format";
import { statusTone } from "../utils/status";
import { DetailGrid } from "./DetailGrid";
import { JsonViewer } from "./JsonViewer";
import { StatusBadge } from "./StatusBadge";

type WorkflowStep = NonNullable<Workflow["steps"]>[number];
type Position = { x: number; y: number };

type DagLayout = {
  positions: Map<string, Position>;
  edges: Array<{ from: string; to: string }>;
  width: number;
  height: number;
};

const nodeWidth = 172;
const nodeHeight = 96;
const xGap = 84;
const yGap = 34;
const margin = 36;

export function Dag({ steps }: { steps: NonNullable<Workflow["steps"]> }) {
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

function PanelHeading({ step }: { step: WorkflowStep }) {
  return (
    <div className="panel-title">
      <h2>{step.step_id}</h2>
      <StatusBadge value={step.status} />
    </div>
  );
}

function buildLayout(steps: WorkflowStep[]): DagLayout {
  const byID = new Map(steps.map((step) => [step.step_id, step]));
  const depthCache = new Map<string, number>();
  const depthFor = (step: WorkflowStep, visiting = new Set<string>()): number => {
    if (depthCache.has(step.step_id)) return depthCache.get(step.step_id) ?? 0;
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

  const edges = steps.flatMap((step) => (step.depends_on ?? []).map((dependency) => ({ from: dependency, to: step.step_id })));
  return {
    positions,
    edges,
    width: Math.max(640, margin * 2 + (maxDepth + 1) * nodeWidth + maxDepth * xGap),
    height: Math.max(280, margin * 2 + maxRows * nodeHeight + Math.max(0, maxRows - 1) * yGap)
  };
}