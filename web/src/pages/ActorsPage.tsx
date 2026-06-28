import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api/client";
import { CodeEditor } from "../components/CodeEditor";
import { FieldError, InlineError } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { ActorTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import { counterSource } from "../utils/examples";
import { defaultID } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { firstValidationError, validateActorCreateForm } from "../utils/formValidation";
import { navigate } from "../utils/navigation";
import { errorMessage } from "../utils/status";

export function ActorsPage() {
  const state = usePolling(() => api.actors(), 1000);
  const [className, setClassName] = useState("Counter");
  const [classSource, setClassSource] = useState(counterSource);
  const [initArgs, setInitArgs] = useState("[0]");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-actor"));
  const [message, setMessage] = useState("");

  const validation = useMemo(() => validateActorCreateForm(className, classSource, initArgs), [className, classSource, initArgs]);

  const create = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    if (!validation.valid) {
      setMessage(firstValidationError(validation.errors));
      return;
    }
    try {
      const actor = await api.createActor({
        class_name: className.trim(),
        class_source: classSource,
        init_args: validation.parsedArgs ?? [],
        init_kwargs: {},
        idempotency_key: idempotencyKey,
        snapshot_every: 25
      });
      navigate(`/actors/${actor.actor_id}`);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title="Actors" />
        <ActorTable rows={state.data?.actors ?? []} />
      </section>
      <form className="panel two-col" onSubmit={create}>
        <div className="form-grid">
          <label>Class name<input value={className} onChange={(event) => setClassName(event.target.value)} aria-invalid={Boolean(validation.errors.className)} className={validation.errors.className ? "input-invalid" : undefined} /><FieldError message={validation.errors.className} /></label>
          <label>Init args<textarea className={`short${validation.errors.args ? " input-invalid" : ""}`} value={initArgs} onChange={(event) => setInitArgs(event.target.value)} aria-invalid={Boolean(validation.errors.args)} /><FieldError message={validation.errors.args} /></label>
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-actor"))}>New key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
            <button type="submit" className="primary" disabled={!validation.valid}>Create</button>
          </div>
          {message && <InlineError message={message} />}
        </div>
        <CodeEditor label="Class source" value={classSource} onChange={setClassSource} error={validation.errors.classSource} />
      </form>
    </div>
  );
}
