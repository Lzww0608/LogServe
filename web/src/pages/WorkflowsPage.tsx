import { useEffect, useMemo, useState } from "react";
import { api, type WorkflowListQuery } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { WorkflowTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { ConsoleSession } from "../types/logserve";
import { roleAtLeast } from "../utils/roles";

const defaultPageSize = 50;

export function WorkflowsPage({ session }: { session?: ConsoleSession | null }) {
  const [status, setStatus] = useState("");
  const [pageSize, setPageSize] = useState(defaultPageSize);
  const [pageIndex, setPageIndex] = useState(0);
  const [pageTokens, setPageTokens] = useState<string[]>([""]);
  const currentPageToken = pageTokens[pageIndex] ?? "";

  useEffect(() => {
    setPageIndex(0);
    setPageTokens([""]);
  }, [status, pageSize]);

  const query = useMemo<WorkflowListQuery>(() => ({
    status,
    limit: pageSize,
    pageToken: currentPageToken
  }), [status, pageSize, currentPageToken]);

  const state = usePolling(() => api.workflows(query), 1000, [status, pageSize, currentPageToken]);
  if (state.error) return <ErrorPanel message={state.error} />;

  const rows = state.data?.workflows ?? [];
  const nextToken = state.data?.next_page_token ?? "";
  const totalCount = state.data?.total_count;

  return (
    <section className="panel">
      <PanelTitle title="Workflows" action={roleAtLeast(session, "operator") ? <a data-nav className="button primary" href="/workflows/new">New</a> : undefined} />
      <div className="toolbar">
        <select value={status} onChange={(event) => setStatus(event.target.value)}>
          <option value="">All status</option>
          <option value="RUNNING">RUNNING</option>
          <option value="COMPLETED">COMPLETED</option>
          <option value="FAILED">FAILED</option>
        </select>
      </div>
      <WorkflowTable rows={rows} pagination={{
        label: pageLabel("workflows", pageIndex, pageSize, rows.length, totalCount),
        pageSize,
        canPrevious: pageIndex > 0,
        canNext: Boolean(nextToken),
        onPrevious: () => setPageIndex((current) => Math.max(0, current - 1)),
        onNext: () => {
          if (!nextToken) return;
          setPageTokens((current) => [...current.slice(0, pageIndex + 1), nextToken]);
          setPageIndex((current) => current + 1);
        },
        onPageSizeChange: setPageSize
      }} />
    </section>
  );
}

function pageLabel(itemName: string, pageIndex: number, pageSize: number, rowCount: number, totalCount?: number): string {
  if (rowCount === 0) return totalCount === 0 ? `0 ${itemName}` : `No ${itemName}`;
  const start = pageIndex * pageSize + 1;
  const end = start + rowCount - 1;
  return totalCount === undefined ? `${start}-${end} ${itemName}` : `${start}-${end} of ${totalCount} ${itemName}`;
}
