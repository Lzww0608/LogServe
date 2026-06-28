import { expect, test, chromium, type Page } from "@playwright/test";
import { mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";

test.describe.configure({ mode: "serial" });

test("settings saves token and sends it on console requests", async ({ page }) => {
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

  await page.getByLabel("API token").fill("browser-token");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("Saved")).toBeVisible();
  await expect(page.evaluate(() => sessionStorage.getItem("logserve.console.token"))).resolves.toBe("browser-token");

  const authorizedRequest = page.waitForRequest((request) =>
    request.url().includes("/api/events") && request.headers().authorization === "Bearer browser-token" && Boolean(request.headers()["x-request-id"])
  );
  await page.getByRole("link", { name: "Overview" }).click();
  await authorizedRequest;
  await expectOverview(page);
});

test("overview renders control-plane snapshot", async ({ page }) => {
  await page.goto("/");
  await expectOverview(page);
  await expectNoDocumentOverflow(page);
});

test("task and workflow lists render paginated API data", async ({ page }) => {
  await page.goto("/tasks");
  await expect(page.getByRole("link", { name: "task-queued-1" })).toBeVisible();
  await expect(page.getByRole("link", { name: "task-success-1" })).toBeVisible();
  await expect(page.getByText("1-3 of 3 tasks")).toBeVisible();

  await page.getByPlaceholder("Worker ID").fill("worker-a");
  await expect(page.getByText("1-2 of 2 tasks")).toBeVisible();

  await page.goto("/tasks?status=FAILED");
  await expect(page.getByRole("link", { name: "task-failed-1" })).toBeVisible();
  await expect(page.getByRole("link", { name: "task-queued-1" })).toHaveCount(0);
  await expect(page.getByText("1-1 of 1 tasks")).toBeVisible();

  await page.goto("/workflows");
  await expect(page.getByRole("link", { name: "simple_add" })).toBeVisible();
  await expect(page.getByText("1-1 of 1 workflows")).toBeVisible();

  await page.goto("/workflows/new");
  await expect(page.locator(".sidebar a.active")).toHaveText("Workflow Builder");
  await expectNoDocumentOverflow(page);
});
test("submit task navigates to task detail", async ({ page }) => {
  await page.goto("/submit/task");
  await expect(page.getByRole("heading", { name: "Submit Task" })).toBeVisible();

  await page.getByLabel("Template").selectOption("sleep");
  await expect(page.getByLabel("Task name")).toHaveValue("sleep_ms");
  await page.getByRole("button", { name: "Format JSON" }).first().click();
  await expect(page.getByLabel("Args JSON", { exact: true })).toHaveValue("[\n  250\n]");

  await page.getByRole("textbox", { name: "Python source" }).focus();
  const editorFocus = await page.locator(".code-editor-frame").evaluate((element) => getComputedStyle(element).boxShadow);
  expect(editorFocus).not.toBe("none");

  await page.getByLabel("Task name").fill("browser add");
  await page.getByRole("button", { name: "Submit" }).click();

  await expect(page).toHaveURL(/\/tasks\/task-ui-1$/);
  await expect(page.getByRole("heading", { name: "Task Detail" })).toBeVisible();
  await expect(page.getByText("task-ui-1")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();
});

test("template library lists and runs a built-in template", async ({ page }) => {
  await page.goto("/templates");
  await expect(page.getByRole("heading", { name: "Templates" })).toBeVisible();
  await expect(page.getByText("Add task")).toBeVisible();
  await expect(page.getByText("Task succeeds with result_json 3.")).toBeVisible();

  const runResponse = page.waitForResponse((response) =>
    response.url().includes("/api/templates/add_task/run") && response.request().method() === "POST"
  );
  await page.locator(".template-card", { hasText: "Add task" }).getByRole("button", { name: "Run" }).click();
  await runResponse;
  await expect(page.getByText("Last run: Add task")).toBeVisible();
  await expect(page.getByText("task-template-1")).toBeVisible();
});
test("viewer role hides operator-only console actions", async ({ page }) => {
  await page.goto("/settings");
  await page.getByLabel("API token").fill("viewer-token");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByTitle("viewer:browser-token")).toBeVisible();

  await page.goto("/templates");
  await expect(page.locator(".template-card", { hasText: "Add task" }).getByRole("button", { name: "Run" })).toBeDisabled();
  await expect(page.locator(".template-card", { hasText: "Mock LLM request" }).getByRole("button", { name: "Run" })).toBeDisabled();

  await page.goto("/tasks");
  await expect(page.getByRole("link", { name: "Submit" })).toHaveCount(0);

  await page.goto("/workflows");
  await expect(page.getByRole("link", { name: "New" })).toHaveCount(0);
});
test("workflow detail renders graphical DAG and step drawer", async ({ page }) => {
  await page.goto("/workflows/wf-1");
  await expect(page.getByRole("heading", { name: "Workflow Detail" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "simple_add" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Step Flow" })).toBeVisible();
  await expect(page.locator("[data-step-id=\"load\"]")).toBeVisible();
  await expect(page.locator("[data-step-id=\"add\"]")).toBeVisible();
  await page.locator("[data-step-id=\"add\"]").click();
  await expect(page.getByLabel("Step detail")).toContainText("add");

  const replayResponse = page.waitForResponse((response) =>
    response.url().includes("/api/workflows/wf-1/replay") && response.request().method() === "POST"
  );
  await page.getByRole("button", { name: "Replay" }).click();
  await replayResponse;
  await expect(page.locator("[data-step-id=\"verify\"]")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Replay" })).toBeVisible();
  await expect(page.getByText("Consistent", { exact: true })).toBeVisible();
  await expectNoDocumentOverflow(page);
});
test("workflow builder validates structured DAG definition", async ({ page }) => {
  await page.goto("/workflows/new");
  await expect(page.getByRole("heading", { name: "Workflow Builder" })).toBeVisible();
  await expect(page.getByText("Template Library")).toBeVisible();
  await expect(page.getByText("DAG Editor")).toBeVisible();

  const validationResponse = page.waitForResponse((response) =>
    response.url().includes("/api/workflows/validate") && response.request().method() === "POST"
  );
  await page.getByRole("button", { name: "Validate" }).click();
  await validationResponse;

  await expect(page.getByText('"valid": true')).toBeVisible();
  await expect(page.getByText("normalized_definition")).toBeVisible();
  await expectNoDocumentOverflow(page);
});

test("LLM page form accepts model and prompt interactions", async ({ page }) => {
  await page.goto("/llm");
  await expect(page.getByRole("heading", { name: "LLM", exact: true })).toBeVisible();
  await expect(page.getByText("model-A:v1")).toBeVisible();

  await page.getByLabel("Model name").fill("model-B");
  await page.getByLabel("Prompt").fill("Explain LogServe browser tests.");
  const registerResponse = page.waitForResponse((response) =>
    response.url().includes("/api/models") && response.request().method() === "POST"
  );
  await page.getByRole("button", { name: "Register" }).click();
  await registerResponse;

  const policyResponse = page.waitForResponse((response) =>
    response.url().includes("/api/admin/scheduling-policy") && response.request().method() === "POST"
  );
  await page.getByRole("button", { name: "Set Policy" }).click();
  await policyResponse;

  await page.getByRole("button", { name: "Submit" }).click();
  await expect(page.getByLabel("Trace task")).toHaveValue("llm-task-1");
  await expect(page.getByText("Submit confirms the task.")).toBeVisible();

  const replayResponse = page.waitForResponse((response) =>
    response.url().includes("/api/llm/llm-task-1/replay") && response.request().method() === "POST"
  );
  await page.getByRole("button", { name: "Replay" }).click();
  await replayResponse;
  await expect(page.getByText("Replay matched the recorded trace.")).toBeVisible();
  await expect(page.getByText("MODEL_LOADED")).toBeVisible();
  await expect(page.getByText("Total latency")).toBeVisible();
});

test("logs page switches stream groups and stream details", async ({ page }) => {
  await page.goto("/logs");
  await expect(page.getByRole("heading", { name: "Logs" })).toBeVisible();
  await expect(page.getByRole("button", { name: "system:functions" })).toBeVisible();
  await expect(page.getByText("Seq 1-2 before 5")).toBeVisible();

  await page.getByLabel("Limit").fill("5000");
  await expect(page.getByLabel("Limit")).toHaveValue("1000");
  await expect(page.getByText("INVALID_ARGUMENT")).toHaveCount(0);

  await page.getByLabel("Filter current page").selectOption("COMMIT");
  await expect(page.getByText("system:functions-commit")).toBeVisible();
  await expect(page.getByText("system:functions-append")).toHaveCount(0);
  await page.getByRole("button", { name: "Inspect" }).click();
  await expect(page.getByRole("heading", { name: "Payload #2" })).toBeVisible();
  await expect(page.getByText('"ok": true')).toBeVisible();

  await page.getByLabel("Filter current page").selectOption("");
  await page.getByRole("button", { name: "Next" }).click();
  await expect(page.getByLabel("From seq")).toHaveValue("3");
  await page.getByRole("button", { name: "Previous" }).click();
  await expect(page.getByLabel("From seq")).toHaveValue("1");

  await page.getByRole("button", { name: "Workflows" }).click();
  await expect(page.getByRole("button", { name: "wf:workflow-1" })).toBeVisible();
  await page.getByRole("button", { name: "wf:workflow-1" }).click();
  await expect(page.getByText("wf:workflow-1-append")).toBeVisible();
  await expectNoDocumentOverflow(page);
});

test("mobile viewport screenshot has no collapsed console layout", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  await expectOverview(page);
  await expectNoDocumentOverflow(page);
  await expectMobileShellLayout(page);
  await page.screenshot({ path: testInfo.outputPath("mobile-overview.png"), fullPage: true });
});

test("Lighthouse accessibility score is at least 90 @lighthouse", async ({ baseURL }, testInfo) => {
  const remoteDebuggingPort = 9222 + testInfo.workerIndex;
  const browser = await chromium.launch({
    args: [
      `--remote-debugging-port=${remoteDebuggingPort}`,
      "--remote-debugging-address=127.0.0.1"
    ]
  });
  try {
    const page = await browser.newPage();
    await page.goto(`${baseURL}/settings`, { waitUntil: "networkidle" });
    await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

    const { default: lighthouse } = await import("lighthouse");
    const result = await lighthouse(`${baseURL}/settings`, {
      port: remoteDebuggingPort,
      onlyCategories: ["accessibility"],
      output: ["json", "html"],
      logLevel: "error"
    });
    const score = result?.lhr.categories.accessibility.score ?? 0;
    const reports = Array.isArray(result?.report) ? result.report : [result?.report ?? ""];
    await writeArtifact(testInfo.outputPath("lighthouse-accessibility.json"), reports[0]);
    await writeArtifact(testInfo.outputPath("lighthouse-accessibility.html"), reports[1] ?? "");
    expect(score).toBeGreaterThanOrEqual(0.9);
  } finally {
    await browser.close();
  }
});

async function expectOverview(page: Page): Promise<void> {
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect(page.getByText("Queue Depth")).toBeVisible();
  await expect(page.getByText("Running Tasks")).toBeVisible();
  await expect(page.getByText("Active Workers")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Recent Tasks" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Workflows" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Workers" })).toBeVisible();
}

async function expectNoDocumentOverflow(page: Page): Promise<void> {
  const overflow = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth
  }));
  expect(overflow.scrollWidth, `document width ${overflow.scrollWidth} should fit viewport ${overflow.viewportWidth}`).toBeLessThanOrEqual(overflow.viewportWidth + 2);
}

async function expectMobileShellLayout(page: Page): Promise<void> {
  const sidebar = await requiredBox(page, ".sidebar");
  const content = await requiredBox(page, ".content");
  const topbar = await requiredBox(page, ".topbar");
  const kpiGrid = await requiredBox(page, ".kpi-grid");
  expect(content.y, "content should stack below mobile sidebar").toBeGreaterThanOrEqual(sidebar.y + sidebar.height - 1);
  expect(kpiGrid.y, "dashboard KPIs should render below the header").toBeGreaterThanOrEqual(topbar.y + topbar.height - 1);
  for (const [name, box] of Object.entries({ sidebar, content, topbar, kpiGrid })) {
    expect(box.width, `${name} should have visible width`).toBeGreaterThan(0);
    expect(box.height, `${name} should have visible height`).toBeGreaterThan(0);
  }
}

async function requiredBox(page: Page, selector: string) {
  const box = await page.locator(selector).boundingBox();
  expect(box, `${selector} should be visible and measurable`).not.toBeNull();
  return box!;
}
async function writeArtifact(path: string, content: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, content, "utf8");
}
