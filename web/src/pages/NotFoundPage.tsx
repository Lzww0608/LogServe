// Fallback route for unmatched console paths.

import { ErrorPanel } from "../components/ErrorPanel";

// Render the fallback error panel for unmatched SPA routes.
export function NotFoundPage() {
  return <ErrorPanel message="Page not found" />;
}
