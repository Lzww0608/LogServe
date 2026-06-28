import type { ReactNode } from "react";
import type { ConsoleSession } from "../types/logserve";
import { Header } from "./Header";
import { Sidebar } from "./Sidebar";

export function AppShell({ path, title, session, sessionError, children }: { path: string; title: string; session?: ConsoleSession | null; sessionError?: string; children: ReactNode }) {
  return (
    <div className="shell">
      <Sidebar path={path} session={session} />
      <main className="content">
        <Header title={title} session={session} sessionError={sessionError} />
        {children}
      </main>
    </div>
  );
}
