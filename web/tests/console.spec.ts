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
    request.url().includes("/api/events") && request.headers().authorization === "Bearer browser-token"
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
  await expect(page.getByText("1-2 of 2 tasks")).toBeVisible();

  await page.getByPlaceholder("Worker ID").fill("worker-a");
  await expect(page.getByText("1-2 of 2 tasks")).toBeVisible();

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

  await page.getByLabel("Task name").fill("browser add");
  await page.getByRole("button", { name: "Submit" }).click();

  await expect(page).toHaveURL(/\/tasks\/task-ui-1$/);
  await expect(page.getByRole("heading", { name: "Task Detail" })).toBeVisible();
  await expect(page.getByText("task-ui-1")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();
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
  await expect(page.getByText("LogServe records workflow events")).toBeVisible();
});

test("logs page switches stream groups and stream details", async ({ page }) => {
  await page.goto("/logs");
  await expect(page.getByRole("heading", { name: "Logs" })).toBeVisible();
  await expect(page.getByRole("button", { name: "system:functions" })).toBeVisible();
  await expect(page.getByText("Seq 1-2 before 3")).toBeVisible();

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
