// Playwright configuration for browser and Lighthouse console tests.

import { defineConfig, devices } from "@playwright/test";

// The runner passes this port to both Vite and Playwright baseURL.
const appPort = Number(process.env.LOGSERVE_PLAYWRIGHT_PORT ?? 5173);
// Browser and Lighthouse invocations write to separate artifact trees.
const artifactKind = process.env.LOGSERVE_PLAYWRIGHT_ARTIFACT_KIND ?? "browser";

// Export the browser-test config consumed by tests/run-playwright.mjs.
export default defineConfig({
  testDir: "./tests",
  // Keep Playwright focused on the browser spec; node:test unit files live beside it.
  testMatch: /console\.spec\.ts/,
  timeout: 45_000,
  expect: {
    // UI assertions wait for mock SSE data and Vite-rendered routes to settle.
    timeout: 7_500
  },
  // Browser and Lighthouse jobs share fixed local ports; keep this file serial by default.
  fullyParallel: false,
  // CI rejects accidental focused tests, while local runs can stay iterative.
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: [
    ["list"],
    // Separate browser and Lighthouse reports so sequential runs do not overwrite artifacts.
    ["html", { outputFolder: `playwright-report/${artifactKind}`, open: "never" }]
  ],
  // Keep screenshots, videos, traces, and Lighthouse artifacts partitioned by run kind.
  outputDir: `test-results/${artifactKind}`,
  use: {
    ...devices["Desktop Chrome"],
    // The runner starts Vite on this URL before invoking Playwright.
    baseURL: `http://127.0.0.1:${appPort}`,
    // Retain diagnostic artifacts only for failing tests to keep local runs small.
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