import type { Workflow } from "../types/logserve";
import { StatusBadge } from "./StatusBadge";

export function Dag({ steps }: { steps: NonNullable<Workflow["steps"]> }) {
  if (!steps.length) return <div className="empty">No steps</div>;
  const edges = steps.flatMap((step) => (step.depends_on ?? []).map((dependency) => ({ dependency, step: step.step_id })));
  return (
    <div className="dag-layout">
      <div className="dag">
        {steps.map((step) => (
          <div className="dag-node" key={step.step_id}>
            <strong>{step.step_id}</strong>
            <span className="subtle">{step.depends_on?.length ? `after ${step.depends_on.join(", ")}` : "root"}</span>
            <StatusBadge value={step.status} />
          </div>
        ))}
      </div>
      <div className="dag-edges">
        {edges.length ? edges.map((edge) => <span key={`${edge.dependency}->${edge.step}`}>{edge.dependency} -&gt; {edge.step}</span>) : <span>No dependencies</span>}
      </div>
    </div>
  );
}
