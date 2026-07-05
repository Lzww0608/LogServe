// Top-bar component that surfaces route title, token state, and current role.

import { getStoredToken } from "../api/client";
import type { ConsoleSession } from "../types/logserve";
import { roleLabel } from "../utils/roles";

// Render control-plane connection metadata and the signed-in role badge.
export function Header({ title, session, sessionError }: { title: string; session?: ConsoleSession | null; sessionError?: string }) {
  // Read token presence at render time so manual refreshes reflect Settings changes.
  const hasToken = Boolean(getStoredToken());
  // This timestamp is a lightweight render marker, not a server heartbeat time.
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
