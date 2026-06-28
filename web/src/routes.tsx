import { ActorDetailPage } from "./pages/ActorDetailPage";
import { ActorsPage } from "./pages/ActorsPage";
import { AdminPage } from "./pages/AdminPage";
import { FunctionsPage } from "./pages/FunctionsPage";
import { LLMPage } from "./pages/LLMPage";
import { LogsPage } from "./pages/LogsPage";
import { NotFoundPage } from "./pages/NotFoundPage";
import { OverviewPage } from "./pages/OverviewPage";
import { SettingsPage } from "./pages/SettingsPage";
import { SubmitTaskPage } from "./pages/SubmitTaskPage";
import { TaskDetailPage } from "./pages/TaskDetailPage";
import { TemplatesPage } from "./pages/TemplatesPage";
import { TasksPage } from "./pages/TasksPage";
import { WorkersPage } from "./pages/WorkersPage";
import { WorkflowBuilderPage } from "./pages/WorkflowBuilderPage";
import { WorkflowDetailPage } from "./pages/WorkflowDetailPage";
import { WorkflowsPage } from "./pages/WorkflowsPage";
import type { ConsoleSession } from "./types/logserve";

export function renderRoute(path: string, routeKey = path, session?: ConsoleSession | null, onSessionChange?: () => void) {
  if (path === "/") return <OverviewPage key={routeKey} session={session} />;
  if (path === "/submit/task") return <SubmitTaskPage key={routeKey} session={session} />;
  if (path === "/templates") return <TemplatesPage key={routeKey} session={session} />;
  if (path === "/tasks") return <TasksPage key={routeKey} session={session} />;
  if (path === "/functions") return <FunctionsPage key={routeKey} />;
  if (path.startsWith("/tasks/")) return <TaskDetailPage key={routeKey} taskID={decodeURIComponent(path.split("/")[2] ?? "")} session={session} />;
  if (path === "/workflows") return <WorkflowsPage key={routeKey} session={session} />;
  if (path === "/workflows/new") return <WorkflowBuilderPage key={routeKey} session={session} />;
  if (path.startsWith("/workflows/")) return <WorkflowDetailPage key={routeKey} workflowID={decodeURIComponent(path.split("/")[2] ?? "")} session={session} />;
  if (path === "/actors") return <ActorsPage key={routeKey} session={session} />;
  if (path.startsWith("/actors/")) return <ActorDetailPage key={routeKey} actorID={decodeURIComponent(path.split("/")[2] ?? "")} session={session} />;
  if (path === "/llm") return <LLMPage key={routeKey} session={session} />;
  if (path === "/workers") return <WorkersPage key={routeKey} />;
  if (path === "/logs") return <LogsPage key={routeKey} />;
  if (path === "/admin") return <AdminPage key={routeKey} session={session} />;
  if (path === "/settings") return <SettingsPage key={routeKey} session={session} onSessionChange={onSessionChange} />;
  return <NotFoundPage key={routeKey} />;
}

export function pathTitle(path: string) {
  if (path === "/") return "Overview";
  if (path.startsWith("/tasks/")) return "Task Detail";
  if (path === "/templates") return "Templates";
  if (path === "/workflows/new") return "Workflow Builder";
  if (path.startsWith("/workflows/") && path !== "/workflows/new") return "Workflow Detail";
  if (path === "/llm") return "LLM";
  if (path.startsWith("/actors/")) return "Actor Detail";
  if (path === "/submit/task") return "Submit Task";
  return path.slice(1).split("/").map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ");
}
