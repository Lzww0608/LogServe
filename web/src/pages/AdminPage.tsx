import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api/client";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, InlineError, Loading } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import type { AdminConfig } from "../types/logserve";
import { formatTime } from "../utils/format";
import { errorMessage } from "../utils/status";

type BackpressureForm = {
  queueHighWatermark: string;
  redeliveryTimeoutMs: string;
  logAppendSlowMs: string;
};

export function AdminPage() {
  const [config, setConfig] = useState<AdminConfig | null>(null);
  const [form, setForm] = useState<BackpressureForm>(emptyBackpressureForm());
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");

  useEffect(() => {
    let cancelled = false;
    const load = async (syncForm: boolean) => {
      try {
        const next = await api.adminConfig();
        if (cancelled) return;
        setConfig(next);
        setError("");
        if (syncForm) setForm(backpressureFormFromConfig(next));
      } catch (loadError) {
        if (!cancelled) setError(errorMessage(loadError));
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    void load(true);
    const timer = window.setInterval(() => void load(false), 2000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  const updateForm = (field: keyof BackpressureForm, value: string) => {
    setForm((current) => ({ ...current, [field]: value }));
    setMessage("");
  };

  const save = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    const queueHighWatermark = parsePositiveInteger(form.queueHighWatermark);
    const redeliveryTimeoutMs = parsePositiveInteger(form.redeliveryTimeoutMs);
    const logAppendSlowMs = parsePositiveInteger(form.logAppendSlowMs);
    if (queueHighWatermark === undefined || redeliveryTimeoutMs === undefined || logAppendSlowMs === undefined) {
      setMessage("Backpressure values must be positive integers");
      return;
    }
    if (queueHighWatermark > 4294967295) {
      setMessage("Queue high watermark exceeds uint32 range");
      return;
    }
    try {
      setSaving(true);
      await api.setBackpressure({ queue_high_watermark: queueHighWatermark, redelivery_timeout_ms: redeliveryTimeoutMs, log_append_slow_ms: logAppendSlowMs });
      const next = await api.adminConfig();
      setConfig(next);
      setForm(backpressureFormFromConfig(next));
      setError("");
      setMessage("Saved");
    } catch (saveError) {
      setMessage(errorMessage(saveError));
    } finally {
      setSaving(false);
    }
  };

  if (loading && !config) return <Loading />;
  if (!config) return <ErrorPanel message={error || "Admin config unavailable"} />;
  const stats = config.metadata_materializer;
  return (
    <div className="stack">
      {error && <InlineError message={error} />}
      <section className="panel">
        <PanelTitle title="Current scheduling policy" action={<span className="badge info">{config.scheduling_policy}</span>} />
        <DetailGrid items={[
          ["Queue high watermark", config.queue_high_watermark],
          ["Redelivery timeout", `${config.redelivery_timeout_ms} ms`],
          ["Log append slow threshold", `${config.log_append_slow_ms} ms`]
        ]} />
      </section>
      <form className="panel form-grid compact" onSubmit={save}>
        <h2>Save backpressure config</h2>
        <label>Queue high watermark<input type="number" min="1" step="1" value={form.queueHighWatermark} onChange={(event) => updateForm("queueHighWatermark", event.target.value)} /></label>
        <label>Redelivery timeout<input type="number" min="1" step="1" value={form.redeliveryTimeoutMs} onChange={(event) => updateForm("redeliveryTimeoutMs", event.target.value)} /></label>
        <label>Log append slow threshold<input type="number" min="1" step="1" value={form.logAppendSlowMs} onChange={(event) => updateForm("logAppendSlowMs", event.target.value)} /></label>
        <button className="primary" type="submit" disabled={saving}>Save backpressure config</button>
        {message && (message === "Saved" ? <span className="subtle">{message}</span> : <InlineError message={message} />)}
      </form>
      <section className="panel split">
        <div>
          <PanelTitle title="Metadata materializer stats" />
          {stats ? <DetailGrid items={[
            ["Mode", stats.mode || "-"],
            ["Pending deltas", stats.pending_deltas ?? 0],
            ["Queued deltas", stats.queued_deltas ?? 0],
            ["Batch max", stats.batch_max ?? 0],
            ["Flush interval", `${stats.flush_interval_ms ?? 0} ms`],
            ["Flush count", stats.flush_count ?? 0],
            ["Flush errors", stats.flush_error_count ?? 0],
            ["Last flush duration", `${stats.last_flush_duration_ms ?? 0} ms`],
            ["Last flush deltas", stats.last_flush_deltas ?? 0],
            ["Lag estimate", `${stats.eventual_lag_estimate_ms ?? 0} ms`],
            ["Last success", formatTime(stats.last_success_at_ms)],
            ["Last error", stats.last_error || "-"]
          ]} /> : <div className="empty">No metadata materializer stats</div>}
        </div>
        <div>
          <PanelTitle title="Compactable log records / bytes" />
          <DetailGrid items={[
            ["Records", config.compactable_log_records],
            ["Bytes", config.compactable_log_bytes]
          ]} />
        </div>
      </section>
    </div>
  );
}

function emptyBackpressureForm(): BackpressureForm {
  return { queueHighWatermark: "", redeliveryTimeoutMs: "", logAppendSlowMs: "" };
}

function backpressureFormFromConfig(config: AdminConfig): BackpressureForm {
  return {
    queueHighWatermark: String(config.queue_high_watermark || ""),
    redeliveryTimeoutMs: String(config.redelivery_timeout_ms || ""),
    logAppendSlowMs: String(config.log_append_slow_ms || "")
  };
}

function parsePositiveInteger(value: string) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) return undefined;
  return parsed;
}
