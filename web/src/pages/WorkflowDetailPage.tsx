import { useCallback, useEffect, useState } from "react";
import { api } from "../api/client";
import { parseEventData, type SSEMessage } from "../api/events";
import { Dag } from "../components/Dag";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, InlineError, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { StepTable } from "../components/domainTables";
import { useEventStream } from "../hooks/useEventStream";
import type { ConsoleSession, Workflow } from "../types/logserve";
import { applyWorkflowEvent } from "../utils/eventState";
import { formatTime } from "../utils/format";
import { roleAtLeast } from "../utils/roles";
import { errorMessage } from "../utils/status";

export function WorkflowDetailPage({ workflowID, session }: { workflowID: string; session?: ConsoleSession | null }) {
  const [workflow, setWorkflow] = useState<Workflow>();
  const [error, setError] = useState("");
  const [replay, setReplay] = useState<unknown>(null);
  const [message, setMessage] = useState("");
  const canReplay = roleAtLeast(session, "operator");

  useEffect(() => {
    setWorkflow(undefined);
    setError("");
    setReplay(null);
    setMessage("");
  }, [workflowID]);
  const handleMessage = useCallback((event: SSEMessage) => {
    if (event.event !== "workflow") return;
    const payload = parseEventData<{ workflow: Workflow }>(event);
    setWorkflow((current) => applyWorkflowEvent(current, payload.workflow));
    setError("");
  }, []);
  useEventStream({ stream: `wf:${workflowID}`, intervalMs: 1000 }, { onMessage: handleMessage, onError: setError }, [workflowID]);

  const runReplay = async () => {
    if (!canReplay) {
      setMessage("Operator role is required to replay workflows.");
      return;
    }
    try {
      const replayResult = await api.replayWorkflow(workflowID);
      setReplay(replayResult);
      if (replayResult.workflow) setWorkflow(replayResult.workflow);
      setMessage("");
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  if (error && !workflow) return <ErrorPanel message={error} />;
  if (!workflow) return <Loading />;
  return (
    <div className="stack">
      {error && <ErrorPanel message={error} />}
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
        <PanelTitle title="Step Flow" action={<button type="button" className="ghost" disabled={!canReplay} onClick={() => void runReplay()}>Replay</button>} />
        <Dag steps={workflow.steps ?? []} />
        {message && <InlineError message={message} />}
        {replay !== null && <ReplaySummary replay={replay} />}
      </section>
      <section className="panel">
        <h2>Steps</h2>
        <StepTable rows={workflow.steps ?? []} />
      </section>
      <section className="panel">
        <h2>Result</h2>
        <JsonViewer title="Workflow Result" value={workflow.result_json ?? workflow.error ?? null} />
      </section>
    </div>
  );
}

function ReplaySummary({ replay }: { replay: unknown }) {
  const consistent = replayConsistency(replay);
  return (
    <div className="trace-result">
      <div className="panel-title">
        <h2>Replay</h2>
        <span className={`badge ${consistent === false ? "bad" : "good"}`}>{consistent === false ? "Diverged" : "Consistent"}</span>
      </div>
      <JsonViewer title="Replay JSON" value={replay} collapsible />
    </div>
  );
}

function replayConsistency(value: unknown): boolean | undefined {
  if (typeof value !== "object" || value === null || !("consistent_with_metadata" in value)) return undefined;
  const consistent = (value as { consistent_with_metadata?: unknown }).consistent_with_metadata;
  return typeof consistent === "boolean" ? consistent : undefined;
}