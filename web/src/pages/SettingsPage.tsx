import { useState } from "react";
import { getStoredToken, setStoredToken } from "../api/client";
import type { ConsoleSession } from "../types/logserve";
import { roleLabel } from "../utils/roles";

export function SettingsPage({ session, onSessionChange }: { session?: ConsoleSession | null; onSessionChange?: () => void }) {
  const [token, setToken] = useState(getStoredToken());
  const [message, setMessage] = useState("");
  const save = () => {
    setStoredToken(token);
    onSessionChange?.();
    setMessage("Saved");
  };
  const clear = () => {
    setToken("");
    setStoredToken("");
    onSessionChange?.();
    setMessage("Cleared");
  };
  return (
    <section className="panel form-grid compact">
      <div className="detail-grid">
        <div>
          <dt>Current role</dt>
          <dd>{roleLabel(session)}</dd>
        </div>
        <div>
          <dt>Subject</dt>
          <dd>{session?.subject ?? "-"}</dd>
        </div>
      </div>
      <label>API token<input type="password" value={token} onChange={(event) => setToken(event.target.value)} /></label>
      <div className="button-row">
        <button className="primary" onClick={save}>Save</button>
        <button className="ghost" onClick={clear}>Clear</button>
      </div>
      {message && <span className="subtle">{message}</span>}
    </section>
  );
}
