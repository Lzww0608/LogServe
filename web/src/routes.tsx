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
import { TasksPage } from "./pages/TasksPage";
import { WorkersPage } from "./pages/WorkersPage";
import { WorkflowBuilderPage } from "./pages/WorkflowBuilderPage";
import { WorkflowDetailPage } from "./pages/WorkflowDetailPage";
import { WorkflowsPage } from "./pages/WorkflowsPage";

export function renderRoute(path: string, routeKey = path) {
  if (path === "/") return <OverviewPage key={routeKey} />;
  if (path === "/submit/task") return <SubmitTaskPage key={routeKey} />;
  if (path === "/tasks") return <TasksPage key={routeKey} />;
  if (path === "/functions") return <FunctionsPage key={routeKey} />;
  if (path.startsWith("/tasks/")) return <TaskDetailPage key={routeKey} taskID={decodeURIComponent(path.split("/")[2] ?? "")} />;
  if (path === "/workflows") return <WorkflowsPage key={routeKey} />;
  if (path === "/workflows/new") return <WorkflowBuilderPage key={routeKey} />;
  if (path.startsWith("/workflows/")) return <WorkflowDetailPage key={routeKey} workflowID={decodeURIComponent(path.split("/")[2] ?? "")} />;
  if (path === "/actors") return <ActorsPage key={routeKey} />;
  if (path.startsWith("/actors/")) return <ActorDetailPage key={routeKey} actorID={decodeURIComponent(path.split("/")[2] ?? "")} />;
  if (path === "/llm") return <LLMPage key={routeKey} />;
  if (path === "/workers") return <WorkersPage key={routeKey} />;
  if (path === "/logs") return <LogsPage key={routeKey} />;
  if (path === "/admin") return <AdminPage key={routeKey} />;
  if (path === "/settings") return <SettingsPage key={routeKey} />;
  return <NotFoundPage key={routeKey} />;
}

export function pathTitle(path: string) {
  if (path === "/") return "Overview";
  if (path.startsWith("/tasks/")) return "Task Detail";
  if (path.startsWith("/workflows/") && path !== "/workflows/new") return "Workflow Detail";
  if (path.startsWith("/actors/")) return "Actor Detail";
  if (path === "/submit/task") return "Submit Task";
  return path.slice(1).split("/").map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ");
}
