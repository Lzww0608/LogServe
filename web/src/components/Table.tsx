import type { ReactNode } from "react";

export type Column<T> = {
  label: string;
  render: (row: T) => ReactNode;
  className?: string;
};

export type TablePagination = {
  label: string;
  pageSize: number;
  pageSizeOptions?: number[];
  canPrevious: boolean;
  canNext: boolean;
  onPrevious: () => void;
  onNext: () => void;
  onPageSizeChange: (pageSize: number) => void;
};

export function Table<T>({ rows, columns, empty, pagination }: { rows: T[]; columns: Column<T>[]; empty: string; pagination?: TablePagination }) {
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
                <tr key={index}>{columns.map((column) => <td key={column.label} className={column.className}>{column.render(row)}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="empty">{empty}</div>
      )}
      {pagination && <PaginationFooter pagination={pagination} />}
    </div>
  );
}

function PaginationFooter({ pagination }: { pagination: TablePagination }) {
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
