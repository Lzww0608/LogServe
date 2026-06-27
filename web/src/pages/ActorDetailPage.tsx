import { useState, type FormEvent } from "react";
import { api } from "../api/client";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, InlineError, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { usePolling } from "../hooks/usePolling";
import { errorMessage } from "../utils/status";

export function ActorDetailPage({ actorID }: { actorID: string }) {
  const state = usePolling(() => api.actor(actorID), 1000, [actorID]);
  const [method, setMethod] = useState("inc");
  const [args, setArgs] = useState("[1]");
  const [callResult, setCallResult] = useState<unknown>(null);
  const [message, setMessage] = useState("");

  const call = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    try {
      setCallResult(await api.callActor(actorID, { method_name: method, args: JSON.parse(args || "[]"), kwargs: {}, timeout_ms: 30000 }));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const actor = state.data;
  if (state.error) return <ErrorPanel message={state.error} />;
  if (!actor) return <Loading />;
  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title={actor.actor_id} action={<StatusBadge value={actor.status} />} />
        <DetailGrid items={[
          ["Class", actor.class_name],
          ["Owner", actor.owner_worker_id],
          ["Epoch", actor.epoch],
          ["Commands", actor.command_count],
          ["Snapshot", actor.snapshot_command_count]
        ]} />
      </section>
      <form className="panel form-grid compact" onSubmit={call}>
        <label>Method<input value={method} onChange={(event) => setMethod(event.target.value)} /></label>
        <label>Args<textarea className="short" value={args} onChange={(event) => setArgs(event.target.value)} /></label>
        <div className="button-row">
          <button className="primary">Call</button>
          <button type="button" className="ghost" onClick={async () => setCallResult(await api.replayActor(actorID))}>Replay</button>
        </div>
        {message && <InlineError message={message} />}
      </form>
      <section className="panel split">
        <div>
          <h2>State</h2>
          <JsonViewer value={actor.state_json ?? null} />
        </div>
        <div>
          <h2>Call Result</h2>
          <JsonViewer value={callResult ?? null} />
        </div>
      </section>
    </div>
  );
}
