export function ErrorPanel({ message }: { message: string }) {
  return <section className="panel error-panel">{message}</section>;
}

export function InlineError({ message }: { message: string }) {
  return <div className="inline-error">{message}</div>;
}

export function Loading() {
  return <section className="panel">Loading</section>;
}
