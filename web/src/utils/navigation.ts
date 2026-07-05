// Push an internal route into browser history and notify the lightweight router.

// navigate mirrors a client-side link click for the custom location-based router.
// navigate is used outside anchor tags where components need imperative route changes.
export function navigate(path: string) {
  window.history.pushState({}, "", path);
  // pushState alone does not emit popstate, so dispatch one for App route recalculation.
  window.dispatchEvent(new PopStateEvent("popstate"));
}
