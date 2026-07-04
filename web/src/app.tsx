// Top-level console component that wires routing, session refresh, and shell layout.

import { useCallback, useEffect, useState } from "react";
import { api } from "./api/client";
import { AppShell } from "./components/AppShell";
import { pathTitle, renderRoute } from "./routes";
import type { ConsoleSession } from "./types/logserve";
import { navigate } from "./utils/navigation";
import { errorMessage } from "./utils/status";

type BrowserLocation = {
  path: string;
  fullPath: string;
};

// Read both route path and full URL so route keys change when query strings change.
function currentBrowserLocation(): BrowserLocation {
  return {
    path: window.location.pathname,
    fullPath: `${window.location.pathname}${window.location.search}${window.location.hash}`
  };
}

// Render the console router and refresh session state after token changes.
export function App() {
  const [location, setLocation] = useState(currentBrowserLocation);
  const [session, setSession] = useState<ConsoleSession | null>(null);
  const [sessionError, setSessionError] = useState("");

  // Reload session metadata after token changes or startup failures.
  const refreshSession = useCallback(async () => {
    try {
      const next = await api.session();
      setSession(next);
      setSessionError("");
    } catch (error) {
      setSession(null);
      setSessionError(errorMessage(error));
    }
  }, []);

  useEffect(() => {
    // Mirror browser history changes into React route state.
    const onPop = () => setLocation(currentBrowserLocation());
    // Intercept same-origin data-nav links so internal navigation stays client-side.
    const onClick = (event: MouseEvent) => {
      const target = event.target as HTMLElement | null;
      const link = target?.closest?.("a[data-nav]") as HTMLAnchorElement | null;
      if (!link || link.origin !== window.location.origin) {
        return;
      }
      event.preventDefault();
      navigate(`${link.pathname}${link.search}${link.hash}`);
    };
    window.addEventListener("popstate", onPop);
    document.addEventListener("click", onClick);
    return () => {
      window.removeEventListener("popstate", onPop);
      document.removeEventListener("click", onClick);
    };
  }, []);

  useEffect(() => {
    void refreshSession();
    window.addEventListener("logserve:token-change", refreshSession);
    return () => window.removeEventListener("logserve:token-change", refreshSession);
  }, [refreshSession]);

  return (
    <AppShell path={location.path} title={pathTitle(location.path)} session={session} sessionError={sessionError}>
      {renderRoute(location.path, location.fullPath, session, refreshSession)}
    </AppShell>
  );
}
