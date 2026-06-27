import { useEffect, useState } from "react";
import { AppShell } from "./components/AppShell";
import { pathTitle, renderRoute } from "./routes";
import { navigate } from "./utils/navigation";

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

  return (
    <AppShell path={location.path} title={pathTitle(location.path)}>
      {renderRoute(location.path, location.fullPath)}
    </AppShell>
  );
}