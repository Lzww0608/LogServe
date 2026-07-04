// Log browser route for stream discovery, sequence pagination, and payload inspection.

import { useEffect, useMemo, useState } from "react";
import { api } from "../api/client";
import { InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { LogRecordTable, LogStreamTable, StreamStatsPanel } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { LogRecord, LogStreamDetail } from "../types/logserve";
import { copyToClipboard } from "../utils/clipboard";
import { payloadPreview } from "../utils/format";
import { errorMessage } from "../utils/status";

const defaultRecordPageSize = 50;
const maxRecordPageSize = 1000;
const detailPollIntervalMs = 1000;
const prefixTabs = [
  { label: "All", value: "" },
  { label: "System", value: "system:" },
  { label: "Workflows", value: "wf:" },
  { label: "Actors", value: "actor:" },
  { label: "LLM", value: "llm:" }
];

// Render log stream search, sequence pagination, and payload inspection.
export function LogsPage() {
  const [prefix, setPrefix] = useState("system:");
  const [streamQuery, setStreamQuery] = useState("");
  const [eventType, setEventType] = useState("");
  const [selectedStream, setSelectedStream] = useState("");
  const [detail, setDetail] = useState<LogStreamDetail>();
  const [detailError, setDetailError] = useState("");
  const [recordPageSize, setRecordPageSize] = useState(defaultRecordPageSize);
  const [recordPageIndex, setRecordPageIndex] = useState(0);
  // Store the starting sequence for each visited page because log pagination is next_seq-based.
  const [recordFromSeqs, setRecordFromSeqs] = useState<number[]>([1]);
  const [fromSeqText, setFromSeqText] = useState("1");
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [selectedRecord, setSelectedRecord] = useState<LogRecord | null>(null);
  const currentFromSeq = recordFromSeqs[recordPageIndex] ?? 1;
  const streamsState = usePolling(() => api.logStreams(prefix.trim()), autoRefresh ? 2000 : 0, [prefix, autoRefresh]);

  const streamIDs = streamsState.data?.stream_ids ?? [];
  const visibleStreamIDs = useMemo(() => {
    const query = streamQuery.trim().toLowerCase();
    return query ? streamIDs.filter((streamID) => streamID.toLowerCase().includes(query)) : streamIDs;
  }, [streamIDs, streamQuery]);

  useEffect(() => {
    if (!visibleStreamIDs.length) {
      if (selectedStream) setSelectedStream("");
      return;
    }
    if (!selectedStream || !visibleStreamIDs.includes(selectedStream)) {
      setSelectedStream(visibleStreamIDs[0]);
    }
  }, [visibleStreamIDs, selectedStream]);

  useEffect(() => {
    setDetail(undefined);
    setDetailError("");
    setRecordPageIndex(0);
    setRecordFromSeqs([1]);
    setFromSeqText("1");
    setEventType("");
    setSelectedRecord(null);
  }, [selectedStream, recordPageSize]);

  useEffect(() => {
    if (!selectedStream) {
      setDetail(undefined);
      return;
    }
    let cancelled = false;
    let timer: number | undefined;
    // Load one log-record page and ignore late responses after stream changes.
    const loadDetail = async () => {
      try {
        const next = await api.logStream(selectedStream, currentFromSeq, recordPageSize);
        if (!cancelled) {
          setDetail(next);
          setDetailError("");
        }
      } catch (error) {
        if (!cancelled) setDetailError(errorMessage(error));
      }
    };
    void loadDetail();
    if (autoRefresh) timer = window.setInterval(loadDetail, detailPollIntervalMs);
    return () => {
      cancelled = true;
      if (timer) window.clearInterval(timer);
    };
  }, [selectedStream, currentFromSeq, recordPageSize, autoRefresh]);

  const statsByStream = useMemo(() => {
    const entries = streamsState.data?.stats ?? [];
    return new Map(entries.map((item) => [item.stream_id, item]));
  }, [streamsState.data]);
  const stats = statsByStream.get(selectedStream) ?? detail?.stats;
  const rows = detail?.records ?? [];
  const eventTypes = useMemo(() => Array.from(new Set(rows.map((row) => row.event_type).filter(Boolean))) as string[], [rows]);
  const filteredRows = eventType ? rows.filter((row) => row.event_type === eventType) : rows;
  const nextSeq = detail?.next_seq ?? nextSeqFromRows(rows, currentFromSeq);
  const canNext = nextSeq > currentFromSeq && (Boolean(detail?.has_more) || (stats?.next_seq !== undefined && nextSeq < stats.next_seq));

  // Reset record pagination to a user-entered sequence number.
  const jumpToSeq = () => {
    const parsed = Number(fromSeqText);
    if (!Number.isInteger(parsed) || parsed < 1) return;
    setRecordFromSeqs([parsed]);
    setRecordPageIndex(0);
    setEventType("");
    setSelectedRecord(null);
  };

  return (
    <div className="stack">
      <section className="panel">
        <div className="toolbar filter-toolbar">
          <div className="quick-tabs" aria-label="Stream prefix tabs">
            {prefixTabs.map((tab) => <button type="button" key={tab.label} className={`ghost${prefix === tab.value ? " active" : ""}`} onClick={() => setPrefix(tab.value)}>{tab.label}</button>)}
          </div>
          <input value={prefix} onChange={(event) => setPrefix(event.target.value)} placeholder="Stream prefix" aria-label="Stream prefix" />
          <input value={streamQuery} onChange={(event) => setStreamQuery(event.target.value)} placeholder="Search streams" aria-label="Search streams" />
          <label className="checkbox-row"><input type="checkbox" checked={autoRefresh} onChange={(event) => setAutoRefresh(event.target.checked)} /> Auto refresh</label>
        </div>
        {streamsState.error && <InlineError message={streamsState.error} />}
        <LogStreamTable streamIDs={visibleStreamIDs} stats={statsByStream} selected={selectedStream} onSelect={setSelectedStream} />
      </section>
      <section className="panel">
        <PanelTitle title={selectedStream || "Stream Detail"} action={selectedStream ? <button type="button" className="ghost compact-button" onClick={() => void copyToClipboard(selectedStream)}>Copy stream id</button> : undefined} />
        {detailError && <InlineError message={detailError} />}
        <div className="toolbar filter-toolbar">
          <label>From seq<input type="number" min="1" value={fromSeqText} onChange={(event) => setFromSeqText(event.target.value)} /></label>
          <label>Limit<input type="number" min="1" max={maxRecordPageSize} value={recordPageSize} onChange={(event) => setRecordPageSize(clampRecordPageSize(event.target.value))} /></label>
          <label>Filter current page<select value={eventType} onChange={(event) => setEventType(event.target.value)}>
            <option value="">All current page events</option>
            {eventTypes.map((type) => <option key={type} value={type}>{type}</option>)}
          </select></label>
          <button type="button" className="ghost" onClick={jumpToSeq}>Load</button>
        </div>
        <div className="log-detail-grid">
          <div className="stack">
            <StreamStatsPanel stats={stats} />
            <LogRecordTable rows={filteredRows} onInspect={setSelectedRecord} onCopyPayload={(record) => void copyToClipboard(payloadPreview(record))} pagination={{
              label: logPageLabel(rows, currentFromSeq, stats?.next_seq),
              pageSize: recordPageSize,
              canPrevious: recordPageIndex > 0,
              canNext,
              onPrevious: () => {
                const previousIndex = Math.max(0, recordPageIndex - 1);
                setRecordPageIndex(previousIndex);
                setFromSeqText(String(recordFromSeqs[previousIndex] ?? 1));
                setEventType("");
                setSelectedRecord(null);
              },
              onNext: () => {
                if (!canNext) return;
                setRecordFromSeqs((current) => [...current.slice(0, recordPageIndex + 1), nextSeq]);
                setFromSeqText(String(nextSeq));
                setRecordPageIndex((current) => current + 1);
                setEventType("");
                setSelectedRecord(null);
              },
              onPageSizeChange: setRecordPageSize
            }} />
          </div>
          <PayloadDrawer record={selectedRecord} />
        </div>
      </section>
    </div>
  );
}

// Render the selected log record payload beside the record table.
function PayloadDrawer({ record }: { record: LogRecord | null }) {
  if (!record) return <aside className="drawer-inline"><strong>Payload</strong><span className="subtle">Select a log record to inspect its payload.</span></aside>;
  return (
    <aside className="drawer-inline">
      <PanelTitle title={`Payload #${record.seq}`} action={<button type="button" className="ghost compact-button" onClick={() => void copyToClipboard(payloadPreview(record))}>Copy payload</button>} />
      <JsonViewer title="Payload JSON" value={payloadValue(record)} />
    </aside>
  );
}

// Choose the richest payload representation exposed by the backend DTO.
function payloadValue(record: LogRecord): unknown {
  if (record.payload_json !== undefined) return record.payload_json;
  if (record.payload_text !== undefined) return record.payload_text;
  if (record.payload_base64 !== undefined) return { base64: record.payload_base64 };
  return null;
}

// Infer the next request sequence when the backend omits next_seq.
function nextSeqFromRows(rows: LogStreamDetail["records"], fallback: number): number {
  if (!rows.length) return fallback;
  return rows[rows.length - 1].seq + 1;
}

// Describe the visible sequence range relative to the stream tail.
function logPageLabel(rows: LogStreamDetail["records"], requestedFromSeq: number, streamNextSeq?: number): string {
  if (rows.length === 0) return streamNextSeq ? `No records from seq ${requestedFromSeq}` : "No records";
  const startSeq = rows[0].seq;
  const endSeq = rows[rows.length - 1].seq;
  return streamNextSeq === undefined ? `Seq ${startSeq}-${endSeq}` : `Seq ${startSeq}-${endSeq} before ${streamNextSeq}`;
}
// Constrain requested log page size to the backend-supported range.
function clampRecordPageSize(value: string | number): number {
  const parsed = Number(value);
  if (!Number.isInteger(parsed)) return defaultRecordPageSize;
  return Math.min(maxRecordPageSize, Math.max(1, parsed));
}