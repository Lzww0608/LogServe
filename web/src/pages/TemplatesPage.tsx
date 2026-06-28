import { useEffect, useState } from "react";
import { api } from "../api/client";
import { ErrorPanel, InlineError, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import type { ConsoleSession, TemplateInfo, TemplateRunResponse } from "../types/logserve";
import { defaultID } from "../utils/format";
import { roleAtLeast } from "../utils/roles";
import { errorMessage } from "../utils/status";

export function TemplatesPage({ session }: { session?: ConsoleSession | null }) {
  const [templates, setTemplates] = useState<TemplateInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [running, setRunning] = useState("");
  const [runError, setRunError] = useState("");
  const [result, setResult] = useState<TemplateRunResponse | null>(null);
  const canRunAny = roleAtLeast(session, "operator");

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const payload = await api.templates();
        if (cancelled) return;
        setTemplates(payload.templates);
        setError("");
      } catch (loadError) {
        if (!cancelled) setError(errorMessage(loadError));
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, []);

  const run = async (template: TemplateInfo) => {
    if (!roleAtLeast(session, template.required_role)) {
      const required = template.required_role.charAt(0).toUpperCase() + template.required_role.slice(1);
      setRunError(`${required} role is required to run this template.`);
      return;
    }
    setRunning(template.id);
    setRunError("");
    try {
      setResult(await api.runTemplate(template.id, { idempotency_key: defaultID(`ui-template-${template.id}`) }));
    } catch (runError) {
      setRunError(errorMessage(runError));
    } finally {
      setRunning("");
    }
  };

  if (loading) return <Loading />;
  if (error) return <ErrorPanel message={error} />;

  return (
    <div className="stack">
      {!canRunAny && <InlineError message="Viewer role can inspect templates; operator role is required to run most templates." />}
      <section className="panel">
        <PanelTitle title="Template Library" />
        <div className="template-library-grid">
          {templates.map((template) => {
            const canRunTemplate = roleAtLeast(session, template.required_role);
            return <article className="template-card" key={template.id}>
              <div>
                <div className="template-card-title">
                  <h2>{template.label}</h2>
                  <span className="badge info">{template.kind}</span>
                </div>
                <p>{template.description}</p>
                <dl>
                  <dt>Expected result</dt>
                  <dd>{template.expected_result}</dd>
                </dl>
              </div>
              <button type="button" className="primary" disabled={!canRunTemplate || running !== ""} onClick={() => void run(template)}>
                {running === template.id ? "Running" : "Run"}
              </button>
            </article>;
          })}
        </div>
      </section>
      {runError && <InlineError message={runError} />}
      {result && <section className="panel">
        <PanelTitle title={`Last run: ${result.template.label}`} />
        <JsonViewer value={result.result} />
      </section>}
    </div>
  );
}