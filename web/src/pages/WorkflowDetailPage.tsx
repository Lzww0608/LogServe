import { useState } from "react";
import { api } from "../api/client";
import { Dag } from "../components/Dag";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, InlineError, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { StepTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import { formatTime } from "../utils/format";
import { errorMessage } from "../utils/status";

export function WorkflowDetailPage({ workflowID }: { workflowID: string }) {
  const state = usePolling(() => api.workflow(workflowID), 1000, [workflowID]);
  const [replay, setReplay] = useState<unknown>(null);
  const [message, setMessage] = useState("");
  if (state.error) return <ErrorPanel message={state.error} />;
  const workflow = state.data;
  if (!workflow) return <Loading />;
  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title={workflow.workflow_name || workflow.workflow_id} action={<StatusBadge value={workflow.status} />} />
        <DetailGrid items={[
          ["Workflow ID", workflow.workflow_id],
          ["Latency", workflow.latency_ms ? `${workflow.latency_ms} ms` : "-"],
          ["Created", formatTime(workflow.created_at_ms)],
          ["Updated", formatTime(workflow.updated_at_ms)]
        ]} />
      </section>
      <section className="panel">
        <PanelTitle title="Step Flow" action={<button className="ghost" onClick={async () => {
          try {
            setReplay(await api.replayWorkflow(workflowID));
            setMessage("");
          } catch (error) {
            setMessage(errorMessage(error));
          }
        }}>Replay</button>} />
        <Dag steps={workflow.steps ?? []} />
        {message && <InlineError message={message} />}
        {replay !== null && <JsonViewer value={replay} />}
      </section>
      <section className="panel">
        <h2>Steps</h2>
        <StepTable rows={workflow.steps ?? []} />
      </section>
      <section className="panel">
        <h2>Result</h2>
        <JsonViewer value={workflow.result_json ?? workflow.error ?? null} />
      </section>
    </div>
  );
}
