import { defineConfig, devices } from "@playwright/test";

const appPort = Number(process.env.LOGSERVE_PLAYWRIGHT_PORT ?? 5173);
const artifactKind = process.env.LOGSERVE_PLAYWRIGHT_ARTIFACT_KIND ?? "browser";

export default defineConfig({
  testDir: "./tests",
  testMatch: /console\.spec\.ts/,
  timeout: 45_000,
  expect: {
    timeout: 7_500
  },
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: [
    ["list"],
    ["html", { outputFolder: `playwright-report/${artifactKind}`, open: "never" }]
  ],
  outputDir: `test-results/${artifactKind}`,
  use: {
    ...devices["Desktop Chrome"],
    baseURL: `http://127.0.0.1:${appPort}`,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure"
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"]
      }
    }
  ]
});