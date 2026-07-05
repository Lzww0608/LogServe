$ErrorActionPreference = "Stop"

# Runs the simple_rag workflow example against a disposable logserve-dev
# runtime. It exercises workflow scheduling and task execution without relying
# on any long-lived local LogServe services.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-workflow-smoke-" + [guid]::NewGuid().ToString("N"))
$logAddr = "127.0.0.1:56051"
$controlAddr = "127.0.0.1:56052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr

# logserve-dev is used as the process boundary so the workflow smoke can start
# and stop the complete local runtime with one handle.
$proc = Start-Process `
  -FilePath "go" `
  -ArgumentList @(
    "run",
    "./cmd/logserve-dev",
    "--log-addr",
    $logAddr,
    "--control-addr",
    $controlAddr,
    "--data-dir",
    $dataDir,
    "--executor",
    (Join-Path $root "executor\python\server.py")
  ) `
  -WorkingDirectory $root `
  -WindowStyle Hidden `
  -PassThru

try {
  # Keep startup synchronization simple: the example is only launched after a
  # fixed delay for the embedded control and worker services.
  Start-Sleep -Seconds 4
  python (Join-Path $root "examples\simple_rag\workflow.py")
}
finally {
  # Always stop the hidden runtime so a failed workflow example does not leave
  # fixed loopback ports occupied for the next smoke run.
  if ($proc -and -not $proc.HasExited) {
    Stop-Process -Id $proc.Id -Force
  }
}
