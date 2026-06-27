import { useState } from "react";
import { getStoredToken, setStoredToken } from "../api/client";

export function SettingsPage() {
  const [token, setToken] = useState(getStoredToken());
  const [message, setMessage] = useState("");
  return (
    <section className="panel form-grid compact">
      <label>API token<input type="password" value={token} onChange={(event) => setToken(event.target.value)} /></label>
      <div className="button-row">
        <button className="primary" onClick={() => {
          setStoredToken(token);
          setMessage("Saved");
        }}>Save</button>
        <button className="ghost" onClick={() => {
          setToken("");
          setStoredToken("");
          setMessage("Cleared");
        }}>Clear</button>
      </div>
      {message && <span className="subtle">{message}</span>}
    </section>
  );
}
