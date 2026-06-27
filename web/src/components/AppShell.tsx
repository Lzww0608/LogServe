import type { ReactNode } from "react";
import { Header } from "./Header";
import { Sidebar } from "./Sidebar";

export function AppShell({ path, title, children }: { path: string; title: string; children: ReactNode }) {
  return (
    <div className="shell">
      <Sidebar path={path} />
      <main className="content">
        <Header title={title} />
        {children}
      </main>
    </div>
  );
}
