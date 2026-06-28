import { getStoredToken } from "../api/client";
import type { ConsoleSession } from "../types/logserve";
import { roleLabel } from "../utils/roles";

export function Header({ title, session, sessionError }: { title: string; session?: ConsoleSession | null; sessionError?: string }) {
  const hasToken = Boolean(getStoredToken());
  const refreshedAt = new Date().toLocaleTimeString();
  return (
    <header className="topbar">
      <div>
        <h1>{title}</h1>
        <div className="header-meta" aria-label="Control plane state">
          <span className={`status-dot ${session ? "good" : "warn"}`} />
          <span>HTTP gateway</span>
          <span>{hasToken ? "Token set" : "No token"}</span>
          <span>Refreshed {refreshedAt}</span>
        </div>
      </div>
      <div className="topbar-actions">
        <span className={`badge ${session ? "info" : "warn"}`} title={sessionError || session?.subject || undefined}>{roleLabel(session)}</span>
        <button type="button" className="ghost" onClick={() => window.location.reload()}>Refresh</button>
      </div>
    </header>
  );
}