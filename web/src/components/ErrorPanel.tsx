export function ErrorPanel({ message }: { message: string }) {
  return <section className="panel error-panel">{message}</section>;
}

export function InlineError({ message }: { message: string }) {
  return <div className="inline-error">{message}</div>;
}

export function FieldError({ message }: { message?: string }) {
  if (!message) return null;
  return <span className="field-error">{message}</span>;
}

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