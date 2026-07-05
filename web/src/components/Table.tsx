// Generic table primitive for typed rows and optional API pagination controls.

import type { ReactNode } from "react";

// Column describes how one typed row field is rendered into a table cell.
export type Column<T> = {
  label: string;
  render: (row: T) => ReactNode;
  className?: string;
};

// TablePagination is UI state supplied by pages that own cursor or offset pagination.
export type TablePagination = {
  label: string;
  pageSize: number;
  pageSizeOptions?: number[];
  canPrevious: boolean;
  canNext: boolean;
  // Navigation callbacks are supplied by the page so cursor and offset pagination both fit this primitive.
  onPrevious: () => void;
  onNext: () => void;
  // Page-size changes reset or reload data in the owning page; the table only emits the selected size.
  onPageSizeChange: (pageSize: number) => void;
};

// Render rows through caller-provided column renderers, including the empty state.
export function Table<T>({ rows, columns, empty, pagination }: { rows: T[]; columns: Column<T>[]; empty: ReactNode; pagination?: TablePagination }) {
  return (
    <div className="table-shell">
      {rows.length ? (
        <div className="table-wrap sticky-table">
          <table>
            <thead>
              <tr>{columns.map((column) => <th key={column.label} className={column.className}>{column.label}</th>)}</tr>
            </thead>
            <tbody>
              {rows.map((row, index) => (
                // Rows do not require ids because callers pass already-windowed lists and cells are read-only.
                <tr key={index}>{columns.map((column) => <td key={column.label} className={column.className}>{column.render(row)}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="empty empty-state">{empty}</div>
      )}
      {pagination && <PaginationFooter pagination={pagination} />}
    </div>
  );
}

// Render page-size and previous/next controls for token or offset pagination.
function PaginationFooter({ pagination }: { pagination: TablePagination }) {
  // Default page sizes match the backend list limits used by console pages.
  const options = pagination.pageSizeOptions ?? [25, 50, 100];
  return (
    <div className="table-footer">
      <span className="pagination-summary">{pagination.label}</span>
      <div className="pagination-controls">
        <label className="page-size-control">
          <span>Rows</span>
          <select value={pagination.pageSize} onChange={(event) => pagination.onPageSizeChange(Number(event.target.value))}>
            {options.map((option) => <option key={option} value={option}>{option}</option>)}
          </select>
        </label>
        <button type="button" className="ghost compact-button" disabled={!pagination.canPrevious} onClick={pagination.onPrevious}>Previous</button>
        <button type="button" className="ghost compact-button" disabled={!pagination.canNext} onClick={pagination.onNext}>Next</button>
      </div>
    </div>
  );
}
