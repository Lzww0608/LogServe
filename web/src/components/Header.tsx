import type { ConsoleSession } from "../types/logserve";
import { roleLabel } from "../utils/roles";

export function Header({ title, session, sessionError }: { title: string; session?: ConsoleSession | null; sessionError?: string }) {
  return (
    <header className="topbar">
      <div>
        <h1>{title}</h1>
        <span className="subtle">Control plane: HTTP gateway</span>
      </div>
      <div className="topbar-actions">
        <span className={`badge ${session ? "info" : "warn"}`} title={sessionError || session?.subject || undefined}>{roleLabel(session)}</span>
        <button className="ghost" onClick={() => window.location.reload()}>Refresh</button>
      </div>
    </header>
  );
}
