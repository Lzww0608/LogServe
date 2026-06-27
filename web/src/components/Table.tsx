import type { ReactNode } from "react";

export type Column<T> = {
  label: string;
  render: (row: T) => ReactNode;
  className?: string;
};

export function Table<T>({ rows, columns, empty }: { rows: T[]; columns: Column<T>[]; empty: string }) {
  if (!rows.length) return <div className="empty">{empty}</div>;
  return (
    <div className="table-wrap">
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
  );
}
