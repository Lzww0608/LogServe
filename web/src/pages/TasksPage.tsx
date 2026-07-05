// Task list route with filters and token-based pagination.

import { useEffect, useMemo, useState } from "react";
import { api, type TaskListQuery } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { TaskTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { ConsoleSession } from "../types/logserve";
import { roleAtLeast } from "../utils/roles";

// Match the backend-friendly default while still allowing URL query overrides.
const defaultPageSize = 50;

// Render task filters and token-based list pagination.
export function TasksPage({ session }: { session?: ConsoleSession | null }) {
  const initialFilters = taskFiltersFromSearch();
  const [query, setQuery] = useState(initialFilters.query);
  const [status, setStatus] = useState(initialFilters.status);
  const [workerID, setWorkerID] = useState(initialFilters.workerID);
  const [workflowID, setWorkflowID] = useState(initialFilters.workflowID);
  const [pageSize, setPageSize] = useState(initialFilters.pageSize);
  const [pageIndex, setPageIndex] = useState(0);
  // Keep previously returned page tokens so Previous can work with token-based pagination.
  const [pageTokens, setPageTokens] = useState<string[]>([""]);

  const trimmedQuery = query.trim();
  const trimmedWorkerID = workerID.trim();
  const trimmedWorkflowID = workflowID.trim();
  const currentPageToken = pageTokens[pageIndex] ?? "";

  // Any filter change invalidates previously returned opaque page tokens.
  useEffect(() => {
    setPageIndex(0);
    setPageTokens([""]);
  }, [trimmedQuery, status, trimmedWorkerID, trimmedWorkflowID, pageSize]);

  // Memoize the API query object so polling dependencies reflect the opaque page token and active filters.
  const taskQuery = useMemo<TaskListQuery>(() => ({
    q: trimmedQuery,
    status,
    workerID: trimmedWorkerID,
    workflowID: trimmedWorkflowID,
    limit: pageSize,
    pageToken: currentPageToken
  }), [trimmedQuery, status, trimmedWorkerID, trimmedWorkflowID, pageSize, currentPageToken]);

  const state = usePolling(() => api.tasks(taskQuery), 1000, [trimmedQuery, status, trimmedWorkerID, trimmedWorkflowID, pageSize, currentPageToken]);
  if (state.error) return <ErrorPanel message={state.error} />;

  const rows = state.data?.tasks ?? [];
  const nextToken = state.data?.next_page_token ?? "";
  const totalCount = state.data?.total_count;

  return (
    <section className="panel">
      <div className="toolbar filter-toolbar">
        <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search tasks" />
        <select value={status} onChange={(event) => setStatus(event.target.value)}>
          <option value="">All status</option>
          <option value="QUEUED">QUEUED</option>
          <option value="RUNNING">RUNNING</option>
          <option value="SUCCEEDED">SUCCEEDED</option>
          <option value="FAILED">FAILED</option>
        </select>
        <input value={workerID} onChange={(event) => setWorkerID(event.target.value)} placeholder="Worker ID" />
        <input value={workflowID} onChange={(event) => setWorkflowID(event.target.value)} placeholder="Workflow ID" />
        {roleAtLeast(session, "operator") && <a data-nav className="button primary" href="/submit/task">Submit</a>}
      </div>
      <TaskTable rows={rows} pagination={{
        label: pageLabel("tasks", pageIndex, pageSize, rows.length, totalCount),
        pageSize,
        canPrevious: pageIndex > 0,
        canNext: Boolean(nextToken),
        onPrevious: () => setPageIndex((current) => Math.max(0, current - 1)),
        onNext: () => {
          if (!nextToken) return;
          // Drop forward-history tokens before storing the next token for this filter set.
          setPageTokens((current) => [...current.slice(0, pageIndex + 1), nextToken]);
          setPageIndex((current) => current + 1);
        },
        onPageSizeChange: setPageSize
      }} />
    </section>
  );
}

// Format a paginated task range from offset-style page state.
function pageLabel(itemName: string, pageIndex: number, pageSize: number, rowCount: number, totalCount?: number): string {
  if (rowCount === 0) return totalCount === 0 ? `0 ${itemName}` : `No ${itemName}`;
  const start = pageIndex * pageSize + 1;
  const end = start + rowCount - 1;
  return totalCount === undefined ? `${start}-${end} ${itemName}` : `${start}-${end} of ${totalCount} ${itemName}`;
}
// Seed task filters and page size from the current URL query string.
function taskFiltersFromSearch() {
  const params = new URLSearchParams(window.location.search);
  const limit = Number(params.get("limit") ?? defaultPageSize);
  return {
    query: params.get("q") ?? "",
    status: params.get("status") ?? "",
    workerID: params.get("worker_id") ?? "",
    workflowID: params.get("workflow_id") ?? "",
    pageSize: Number.isInteger(limit) && limit > 0 ? limit : defaultPageSize
  };
}