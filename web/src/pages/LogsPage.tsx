import { useEffect, useMemo, useState } from "react";
import { api } from "../api/client";
import { InlineError } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { LogRecordTable, LogStreamTable, StreamStatsPanel } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { LogStreamDetail } from "../types/logserve";

export function LogsPage() {
  const [prefix, setPrefix] = useState("system:");
  const [selectedStream, setSelectedStream] = useState("");
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

  const detailState = usePolling<LogStreamDetail>(() => {
    if (!selectedStream) {
      return Promise.resolve({ stream_id: "", from_seq: 1, limit: 100, records: [], stats: null });
    }
    return api.logStream(selectedStream, 1, 100);
  }, 2000, [selectedStream]);

  const statsByStream = useMemo(() => {
    const entries = streamsState.data?.stats ?? [];
    return new Map(entries.map((item) => [item.stream_id, item]));
  }, [streamsState.data]);

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
        {detailState.error && <InlineError message={detailState.error} />}
        <StreamStatsPanel stats={detailState.data?.stats ?? statsByStream.get(selectedStream)} />
        <LogRecordTable rows={detailState.data?.records ?? []} />
      </section>
    </div>
  );
}
