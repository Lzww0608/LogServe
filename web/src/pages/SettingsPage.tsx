// Settings route for browser token management and session refresh.

import { useState } from "react";
import { getStoredToken, setStoredToken } from "../api/client";
import type { ConsoleSession } from "../types/logserve";
import { roleLabel } from "../utils/roles";

// Render token save/clear controls that drive session refresh.
export function SettingsPage({ session, onSessionChange }: { session?: ConsoleSession | null; onSessionChange?: () => void }) {
  const [token, setToken] = useState(getStoredToken());
  const [message, setMessage] = useState("");
  // Save the entered token and trigger a session refresh in the app shell.
  const save = () => {
    // Updating storage alone is not enough; the app shell must re-read /session.
    setStoredToken(token);
    onSessionChange?.();
    setMessage("Saved");
  };
  // Clear the stored token and reset visible session state.
  const clear = () => {
    setToken("");
    // Persist the empty token before notifying the shell so later API calls use viewer access.
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
        <button type="button" className="primary" onClick={save}>Save</button>
        <button type="button" className="ghost" onClick={clear}>Clear</button>
      </div>
      {message && <span className="subtle">{message}</span>}
    </section>
  );
}
