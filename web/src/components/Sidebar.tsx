// Role-filtered navigation sidebar for the single-page console.

import type { ReactNode } from "react";
import type { ConsoleRole, ConsoleSession } from "../types/logserve";
import { roleAtLeast } from "../utils/roles";

// navItems is ordered by the primary console workflow and filtered by minimum role.
const navItems: Array<{ href: string; label: string; minRole?: ConsoleRole }> = [
  { href: "/", label: "Overview", minRole: "viewer" },
  { href: "/submit/task", label: "Submit Task", minRole: "operator" },
  { href: "/templates", label: "Templates", minRole: "viewer" },
  { href: "/tasks", label: "Tasks", minRole: "viewer" },
  { href: "/functions", label: "Functions", minRole: "operator" },
  { href: "/workflows", label: "Workflows", minRole: "viewer" },
  { href: "/workflows/new", label: "Workflow Builder", minRole: "operator" },
  { href: "/actors", label: "Actors", minRole: "operator" },
  { href: "/llm", label: "LLM Serving", minRole: "operator" },
  { href: "/workers", label: "Workers", minRole: "operator" },
  { href: "/logs", label: "Logs", minRole: "viewer" },
  { href: "/admin", label: "Admin", minRole: "admin" },
  { href: "/settings", label: "Settings" }
];

// Render only the navigation entries allowed by the current session role.
export function Sidebar({ path, session }: { path: string; session?: ConsoleSession | null }) {
  return (
    <aside className="sidebar">
      <div className="brand">LogServe</div>
      <nav>
        {/* Settings stays visible without a role so users can enter or clear tokens while signed out. */}
        {navItems.filter((item) => !item.minRole || roleAtLeast(session, item.minRole)).map((item) => (
          <NavLink key={item.href} path={item.href} current={path}>{item.label}</NavLink>
        ))}
      </nav>
    </aside>
  );
}

// Mark a route link active while keeping /workflows/new distinct from workflow details.
function NavLink({ path, current, children }: { path: string; current: string; children: ReactNode }) {
  // Exact matching keeps the workflow builder from also activating the workflow list route.
  const exact = path === "/" || path === "/workflows/new";
  const excludedChild = path === "/workflows" && current === "/workflows/new";
  const active = exact ? current === path : current === path || (!excludedChild && current.startsWith(path + "/"));
  return (
    <a data-nav href={path} className={active ? "active" : ""}>
      {children}
    </a>
  );
}
