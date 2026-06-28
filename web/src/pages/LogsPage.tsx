import { useEffect, useMemo, useState } from "react";
import { api } from "../api/client";
import { InlineError } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { LogRecordTable, LogStreamTable, StreamStatsPanel } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { LogStreamDetail } from "../types/logserve";
import { errorMessage } from "../utils/status";

const defaultRecordPageSize = 50;
const detailPollIntervalMs = 1000;

export function LogsPage() {
  const [prefix, setPrefix] = useState("system:");
  const [selectedStream, setSelectedStream] = useState("");
  const [detail, setDetail] = useState<LogStreamDetail>();
  const [detailError, setDetailError] = useState("");
  const [recordPageSize, setRecordPageSize] = useState(defaultRecordPageSize);
  const [recordPageIndex, setRecordPageIndex] = useState(0);
  const [recordFromSeqs, setRecordFromSeqs] = useState<number[]>([1]);
  const currentFromSeq = recordFromSeqs[recordPageIndex] ?? 1;
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
    setDetail(undefined);
    setDetailError("");
    setRecordPageIndex(0);
    setRecordFromSeqs([1]);
  }, [selectedStream, recordPageSize]);

  useEffect(() => {
    if (!selectedStream) {
      setDetail(undefined);
      return;
    }
    let cancelled = false;
    let timer: number | undefined;
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
    timer = window.setInterval(loadDetail, detailPollIntervalMs);
    return () => {
      cancelled = true;
      if (timer) window.clearInterval(timer);
    };
  }, [selectedStream, currentFromSeq, recordPageSize]);

  const statsByStream = useMemo(() => {
    const entries = streamsState.data?.stats ?? [];
    return new Map(entries.map((item) => [item.stream_id, item]));
  }, [streamsState.data]);
  const stats = statsByStream.get(selectedStream) ?? detail?.stats;
  const rows = detail?.records ?? [];
  const nextSeq = detail?.next_seq ?? nextSeqFromRows(rows, currentFromSeq);
  const canNext = nextSeq > currentFromSeq && (Boolean(detail?.has_more) || (stats?.next_seq !== undefined && nextSeq < stats.next_seq));

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
        <LogRecordTable rows={rows} pagination={{
          label: logPageLabel(rows, currentFromSeq, stats?.next_seq),
          pageSize: recordPageSize,
          canPrevious: recordPageIndex > 0,
          canNext,
          onPrevious: () => setRecordPageIndex((current) => Math.max(0, current - 1)),
          onNext: () => {
            if (!canNext) return;
            setRecordFromSeqs((current) => [...current.slice(0, recordPageIndex + 1), nextSeq]);
            setRecordPageIndex((current) => current + 1);
          },
          onPageSizeChange: setRecordPageSize
        }} />
      </section>
    </div>
  );
}

function nextSeqFromRows(rows: LogStreamDetail["records"], fallback: number): number {
  if (!rows.length) return fallback;
  return rows[rows.length - 1].seq + 1;
}

function logPageLabel(rows: LogStreamDetail["records"], requestedFromSeq: number, streamNextSeq?: number): string {
  if (rows.length === 0) return streamNextSeq ? `No records from seq ${requestedFromSeq}` : "No records";
  const startSeq = rows[0].seq;
  const endSeq = rows[rows.length - 1].seq;
  return streamNextSeq === undefined ? `Seq ${startSeq}-${endSeq}` : `Seq ${startSeq}-${endSeq} before ${streamNextSeq}`;
}