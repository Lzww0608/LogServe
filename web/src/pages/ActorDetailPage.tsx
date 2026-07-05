// Actor detail route for inspecting state, invoking methods, and replaying actor history.

import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api/client";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, FieldError, InlineError, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { usePolling } from "../hooks/usePolling";
import type { ConsoleSession } from "../types/logserve";
import { defaultID } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { firstValidationError, validateActorCallForm } from "../utils/formValidation";
import { roleAtLeast } from "../utils/roles";
import { errorMessage } from "../utils/status";

// Render actor state with method-call and replay actions gated by role.
export function ActorDetailPage({ actorID, session }: { actorID: string; session?: ConsoleSession | null }) {
  // Re-poll when the route actor id changes so stale actor snapshots are not reused across detail pages.
  const state = usePolling(() => api.actor(actorID), 1000, [actorID]);
  const [method, setMethod] = useState("inc");
  const [args, setArgs] = useState("[1]");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-actor-call"));
  const [callResult, setCallResult] = useState<unknown>(null);
  const [message, setMessage] = useState("");
  // Actor calls and replay mutate/inspect runtime state, so the UI mirrors the operator gate.
  const canOperate = roleAtLeast(session, "operator");

  const validation = useMemo(() => validateActorCallForm(method, args), [method, args]);

  // Invoke one actor method and refresh state from the returned actor snapshot.
  const call = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    if (!canOperate) {
      setMessage("Operator role is required to call actors.");
      return;
    }
    if (!validation.valid) {
      setMessage(firstValidationError(validation.errors));
      return;
    }
    try {
      setCallResult(await api.callActor(actorID, {
        method_name: method.trim(),
        args: validation.parsedArgs ?? [],
        kwargs: {},
        idempotency_key: idempotencyKey,
        // Bound interactive calls so the page cannot wait forever on a stuck actor method.
        timeout_ms: 30000
      }));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  // Replay actor history and expose the reconstructed state in the detail view.
  const replay = async () => {
    setMessage("");
    if (!canOperate) {
      setMessage("Operator role is required to replay actors.");
      return;
    }
    try {
      setCallResult(await api.replayActor(actorID));
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
        <label>Method<input value={method} onChange={(event) => setMethod(event.target.value)} aria-invalid={Boolean(validation.errors.method)} className={validation.errors.method ? "input-invalid" : undefined} /><FieldError message={validation.errors.method} /></label>
        <label>Args<textarea className={`short${validation.errors.args ? " input-invalid" : ""}`} value={args} onChange={(event) => setArgs(event.target.value)} aria-invalid={Boolean(validation.errors.args)} /><FieldError message={validation.errors.args} /></label>
        <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
        <div className="button-row">
          <button type="submit" className="primary" disabled={!canOperate || !validation.valid}>Call</button>
          <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-actor-call"))}>New key</button>
          <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
          <button type="button" className="ghost" disabled={!canOperate} onClick={() => void replay()}>Replay</button>
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
