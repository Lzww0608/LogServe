import type { ReactNode } from "react";

const navItems = [
  ["/", "Overview"],
  ["/submit/task", "Submit Task"],
  ["/tasks", "Tasks"],
  ["/functions", "Functions"],
  ["/workflows", "Workflows"],
  ["/workflows/new", "Workflow Builder"],
  ["/actors", "Actors"],
  ["/llm", "LLM Serving"],
  ["/workers", "Workers"],
  ["/logs", "Logs"],
  ["/admin", "Admin"],
  ["/settings", "Settings"]
] as const;

export function Sidebar({ path }: { path: string }) {
  return (
    <aside className="sidebar">
      <div className="brand">LogServe</div>
      <nav>
        {navItems.map(([href, label]) => (
          <NavLink key={href} path={href} current={path}>{label}</NavLink>
        ))}
      </nav>
    </aside>
  );
}

function NavLink({ path, current, children }: { path: string; current: string; children: ReactNode }) {
  const exact = path === "/" || path === "/workflows/new";
  const excludedChild = path === "/workflows" && current === "/workflows/new";
  const active = exact ? current === path : current === path || (!excludedChild && current.startsWith(path + "/"));
  return (
    <a data-nav href={path} className={active ? "active" : ""}>
      {children}
    </a>
  );
}
