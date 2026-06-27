import { useState, type FormEvent } from "react";
import { api } from "../api/client";
import { CodeEditor } from "../components/CodeEditor";
import { InlineError } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { ActorTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import { counterSource } from "../utils/examples";
import { defaultID } from "../utils/format";
import { navigate } from "../utils/navigation";
import { errorMessage } from "../utils/status";

export function ActorsPage() {
  const state = usePolling(() => api.actors(), 1000);
  const [className, setClassName] = useState("Counter");
  const [classSource, setClassSource] = useState(counterSource);
  const [initArgs, setInitArgs] = useState("[0]");
  const [message, setMessage] = useState("");

  const create = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    try {
      const actor = await api.createActor({
        class_name: className,
        class_source: classSource,
        init_args: JSON.parse(initArgs || "[]"),
        init_kwargs: {},
        idempotency_key: defaultID("ui-actor"),
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
          <label>Class name<input value={className} onChange={(event) => setClassName(event.target.value)} /></label>
          <label>Init args<textarea className="short" value={initArgs} onChange={(event) => setInitArgs(event.target.value)} /></label>
          <button className="primary">Create</button>
          {message && <InlineError message={message} />}
        </div>
        <CodeEditor label="Class source" value={classSource} onChange={setClassSource} />
      </form>
    </div>
  );
}
