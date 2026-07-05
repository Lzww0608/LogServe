$ErrorActionPreference = "Stop"

# Runs the hello_task example against a disposable logserve-dev runtime.
# Fixed loopback ports make the script easy to invoke locally, while the unique
# data directory keeps repeated smoke runs from sharing log state.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-task-smoke-" + [guid]::NewGuid().ToString("N"))
$logAddr = "127.0.0.1:55051"
$controlAddr = "127.0.0.1:55052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr

# A single logserve-dev process owns logd, control, and worker services for
# the short-lived task smoke instead of coordinating separate daemons.
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
  # Wait for the embedded services to bind before the Python example submits
  # through LOGSERVE_CONTROL_ADDR.
  Start-Sleep -Seconds 4
  python (Join-Path $root "examples\hello_task\add.py")
}
finally {
  # The process handle belongs to this script, so forced termination cannot
  # affect any externally managed LogServe runtime.
  if ($proc -and -not $proc.HasExited) {
    Stop-Process -Id $proc.Id -Force
  }
}
