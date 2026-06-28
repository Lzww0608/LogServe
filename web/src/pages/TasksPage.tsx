import { useEffect, useMemo, useState } from "react";
import { api, type TaskListQuery } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { TaskTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";

const defaultPageSize = 50;

export function TasksPage() {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("");
  const [workerID, setWorkerID] = useState("");
  const [workflowID, setWorkflowID] = useState("");
  const [pageSize, setPageSize] = useState(defaultPageSize);
  const [pageIndex, setPageIndex] = useState(0);
  const [pageTokens, setPageTokens] = useState<string[]>([""]);

  const trimmedQuery = query.trim();
  const trimmedWorkerID = workerID.trim();
  const trimmedWorkflowID = workflowID.trim();
  const currentPageToken = pageTokens[pageIndex] ?? "";

  useEffect(() => {
    setPageIndex(0);
    setPageTokens([""]);
  }, [trimmedQuery, status, trimmedWorkerID, trimmedWorkflowID, pageSize]);

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
        <a data-nav className="button primary" href="/submit/task">Submit</a>
      </div>
      <TaskTable rows={rows} pagination={{
        label: pageLabel("tasks", pageIndex, pageSize, rows.length, totalCount),
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
