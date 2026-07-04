// Test runner that starts the mock API, Vite, and Playwright in order.

import { spawn, spawnSync } from "node:child_process";
import http from "node:http";

const appPort = Number(process.env.LOGSERVE_PLAYWRIGHT_PORT ?? 5173);
const mockApiPort = Number(process.env.LOGSERVE_MOCK_API_PORT ?? 43080);
const playwrightArgs = process.argv.slice(2);
const artifactKind = isLighthouseRun(playwrightArgs) ? "lighthouse" : "browser";
const children = [];

try {
  const mockApi = start(process.execPath, ["tests/mock-api-server.mjs"], {
    LOGSERVE_MOCK_API_PORT: String(mockApiPort)
  });
  children.push(mockApi);
  await waitForHTTP(`http://127.0.0.1:${mockApiPort}/api/logs/streams`, "mock API", mockApi, '"stream_ids"');

  const vite = start(process.execPath, ["./node_modules/vite/bin/vite.js", "--host", "127.0.0.1", "--port", String(appPort), "--strictPort"], {
    LOGSERVE_WEB_API_PROXY: `http://127.0.0.1:${mockApiPort}`
  });
  children.push(vite);
  await waitForHTTP(`http://127.0.0.1:${appPort}/`, "Vite dev server", vite, '<div id="root"></div>');

  const result = spawnSync(process.execPath, ["./node_modules/@playwright/test/cli.js", "test", "--config=playwright.config.ts", ...playwrightArgs], {
    cwd: process.cwd(),
    env: {
      ...process.env,
      LOGSERVE_PLAYWRIGHT_PORT: String(appPort),
      LOGSERVE_MOCK_API_PORT: String(mockApiPort),
      LOGSERVE_PLAYWRIGHT_ARTIFACT_KIND: artifactKind
    },
    stdio: "inherit"
  });
  process.exitCode = typeof result.status === "number" ? result.status : 1;
  console.log(`[runner] playwright exited with ${process.exitCode}`);
} finally {
  for (const child of children.reverse()) {
    stop(child);
  }
  process.exit(process.exitCode ?? 0);
}

// Spawn a long-lived helper process with inherited env plus test-specific overrides.
function start(command, args, env) {
  const child = spawn(command, args, {
    cwd: process.cwd(),
    env: { ...process.env, ...env },
    stdio: ["ignore", "pipe", "pipe"],
    windowsHide: true
  });
  child.stdout.on("data", (chunk) => process.stdout.write(`[server] ${chunk}`));
  child.stderr.on("data", (chunk) => process.stderr.write(`[server] ${chunk}`));
  return child;
}

// Poll a local HTTP endpoint until the child process is ready or exits early.
function waitForHTTP(url, label, child, expectedText) {
  const deadline = Date.now() + 30_000;
  return new Promise((resolve, reject) => {
    // Probe readiness while also checking whether the child exited early.
    const tryRequest = () => {
      const childExit = childExitError(child, label);
      if (childExit) {
        reject(childExit);
        return;
      }
      const request = http.get(url, (response) => {
        let body = "";
        response.setEncoding("utf8");
        response.on("data", (chunk) => {
          body += chunk;
        });
        response.on("end", () => {
          const exitAfterResponse = childExitError(child, label);
          if (exitAfterResponse) {
            reject(exitAfterResponse);
            return;
          }
          if (response.statusCode && response.statusCode >= 200 && response.statusCode < 300 && body.includes(expectedText)) {
            resolve();
            return;
          }
          retryOrReject(new Error(`${label} returned ${response.statusCode ?? "unknown"} without expected readiness marker`));
        });
      });
      request.once("error", retryOrReject);
      request.setTimeout(2000, () => {
        request.destroy(new Error(`${label} readiness request timed out`));
      });
    };
    // Retry readiness probes until the deadline, then fail with the last observed error.
    const retryOrReject = (error) => {
      if (Date.now() >= deadline) {
        reject(new Error(`${label} was not ready at ${url}: ${error.message}`));
        return;
      }
      setTimeout(tryRequest, 250);
    };
    tryRequest();
  });
}

// Report child-process early exit as a readiness failure.
function childExitError(child, label) {
  if (child.exitCode === null && child.signalCode === null) return null;
  return new Error(`${label} process exited before readiness with code ${child.exitCode ?? "null"} signal ${child.signalCode ?? "null"}`);
}

// Terminate spawned servers, using taskkill on Windows to include child processes.
function stop(child) {
  if (!child.pid || child.killed) return;
  if (process.platform === "win32") {
    spawnSync("taskkill.exe", ["/pid", String(child.pid), "/T", "/F"], { stdio: "ignore" });
    return;
  }
  child.kill("SIGTERM");
}

// Detect Lighthouse-only runs so reports and artifacts are separated from browser tests.
function isLighthouseRun(args) {
  const grepIndex = args.findIndex((arg) => arg === "--grep" || arg.startsWith("--grep="));
  if (grepIndex < 0) return false;
  const grepValue = args[grepIndex].includes("=") ? args[grepIndex].split("=").slice(1).join("=") : args[grepIndex + 1] ?? "";
  return grepValue.includes("@lighthouse");
}