export function Header({ title }: { title: string }) {
  return (
    <header className="topbar">
      <div>
        <h1>{title}</h1>
        <span className="subtle">Control plane: HTTP gateway</span>
      </div>
      <button className="ghost" onClick={() => window.location.reload()}>Refresh</button>
    </header>
  );
}
