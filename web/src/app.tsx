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

function currentBrowserLocation(): BrowserLocation {
  return {
    path: window.location.pathname,
    fullPath: `${window.location.pathname}${window.location.search}${window.location.hash}`
  };
}

export function App() {
  const [location, setLocation] = useState(currentBrowserLocation);
  const [session, setSession] = useState<ConsoleSession | null>(null);
  const [sessionError, setSessionError] = useState("");

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
    const onPop = () => setLocation(currentBrowserLocation());
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
