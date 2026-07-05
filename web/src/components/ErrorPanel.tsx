// Error and loading primitives shared by route pages.

// Render a full-width route error panel.
export function ErrorPanel({ message }: { message: string }) {
  return <section className="panel error-panel">{message}</section>;
}

// Render a compact inline error next to forms or detail panels.
export function InlineError({ message }: { message: string }) {
  return <div className="inline-error">{message}</div>;
}

// Render a field-level validation error only when one is present.
export function FieldError({ message }: { message?: string }) {
  if (!message) return null;
  return <span className="field-error">{message}</span>;
}

// Render an accessible skeleton panel while data is loading.
export function Loading() {
  return (
    <section className="panel loading-panel" aria-busy="true">
      <span className="sr-only">Loading</span>
      <div className="skeleton skeleton-title" />
      <div className="skeleton skeleton-line" />
      <div className="skeleton skeleton-line short" />
    </section>
  );
}
