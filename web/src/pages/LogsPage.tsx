import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "../api/client";
import { parseEventData, type SSEMessage } from "../api/events";
import { InlineError } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { LogRecordTable, LogStreamTable, StreamStatsPanel } from "../components/domainTables";
import { useEventStream } from "../hooks/useEventStream";
import { usePolling } from "../hooks/usePolling";
import type { LogStreamDetail } from "../types/logserve";
import { applyLogRecordsEvent, type LogRecordsEvent } from "../utils/eventState";

export function LogsPage() {
  const [prefix, setPrefix] = useState("system:");
  const [selectedStream, setSelectedStream] = useState("");
  const [detail, setDetail] = useState<LogStreamDetail>();
  const [detailError, setDetailError] = useState("");
  const streamsState = usePolling(() => api.logStreams(prefix.trim()), 2000, [prefix]);

  useEffect(() => {
    const ids = streamsState.data?.stream_ids ?? [];
    if (!ids.length) {
      if (selectedStream) setSelectedStream("");
      return;
    }
    if (!selectedStream || !ids.includes(selectedStream)) {
      setSelectedStream(ids[0]);
    }
  }, [streamsState.data, selectedStream]);

  useEffect(() => {
    setDetailError("");
    setDetail(selectedStream ? { stream_id: selectedStream, from_seq: 1, limit: 100, records: [], stats: null } : undefined);
  }, [selectedStream]);

  const handleLogEvent = useCallback((message: SSEMessage) => {
    if (message.event !== "log_records") return;
    const payload = parseEventData<LogRecordsEvent>(message);
    setDetail((current) => applyLogRecordsEvent(current, payload));
    setDetailError("");
  }, []);
  useEventStream(
    { stream: selectedStream, fromSeq: 1, limit: 100, intervalMs: 1000, records: true, enabled: Boolean(selectedStream) },
    { onMessage: handleLogEvent, onError: setDetailError },
    [selectedStream]
  );

  const statsByStream = useMemo(() => {
    const entries = streamsState.data?.stats ?? [];
    return new Map(entries.map((item) => [item.stream_id, item]));
  }, [streamsState.data]);
  const stats = detail?.stats ?? statsByStream.get(selectedStream);

  return (
    <div className="stack">
      <section className="panel">
        <div className="toolbar">
          <input value={prefix} onChange={(event) => setPrefix(event.target.value)} placeholder="Stream prefix" />
          <button className="ghost" onClick={() => setPrefix("")}>All</button>
          <button className="ghost" onClick={() => setPrefix("system:")}>System</button>
          <button className="ghost" onClick={() => setPrefix("wf:")}>Workflows</button>
          <button className="ghost" onClick={() => setPrefix("actor:")}>Actors</button>
        </div>
        {streamsState.error && <InlineError message={streamsState.error} />}
        <LogStreamTable streamIDs={streamsState.data?.stream_ids ?? []} stats={statsByStream} selected={selectedStream} onSelect={setSelectedStream} />
      </section>
      <section className="panel">
        <PanelTitle title={selectedStream || "Stream Detail"} />
        {detailError && <InlineError message={detailError} />}
        <StreamStatsPanel stats={stats} />
        <LogRecordTable rows={detail?.records ?? []} />
      </section>
    </div>
  );
}
