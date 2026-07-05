$ErrorActionPreference = "Stop"

# Runs the actor counter example against a disposable logserve-dev runtime.
# The script builds logservectl into the temp directory because the Python
# actor SDK shells out through LOGSERVE_CLI for control-plane operations.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-actor-smoke-" + [guid]::NewGuid().ToString("N"))
$cliPath = Join-Path $dataDir "logservectl.exe"
$logAddr = "127.0.0.1:57051"
$controlAddr = "127.0.0.1:57052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr
$env:LOGSERVE_CLI = $cliPath

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
go build -o $cliPath ./cmd/logservectl

# Start logserve-dev as one hidden process so logd, control, and worker state
# share the same temporary data directory for this smoke run.
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
  # logserve-dev has no readiness probe here; the fixed wait is a pragmatic
  # buffer before the example submits actor commands through the SDK.
  Start-Sleep -Seconds 4
  python (Join-Path $root "examples\actor_counter\counter.py")
}
finally {
  # Clean up only the process and CLI created by this smoke script; the temp
  # data directory is left to the OS temp cleanup policy.
  if ($proc -and -not $proc.HasExited) {
    Stop-Process -Id $proc.Id -Force
  }
  if (Test-Path -LiteralPath $cliPath) {
    Remove-Item -LiteralPath $cliPath -Force -ErrorAction SilentlyContinue
  }
}
