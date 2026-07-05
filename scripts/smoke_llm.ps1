$ErrorActionPreference = "Stop"

# Runs the RAG/LLM workflow example against explicit logd, control, and worker
# processes. Separate workers advertise different model sets so routing and
# model-aware scheduling paths are covered by the smoke run.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-llm-smoke-" + [guid]::NewGuid().ToString("N"))
$cliPath = Join-Path $dataDir "logservectl.exe"
$logAddr = "127.0.0.1:58051"
$controlAddr = "127.0.0.1:58052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr
$env:LOGSERVE_CLI = $cliPath

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
go build -o $cliPath ./cmd/logservectl

$procs = @()

# Start-LogServeProcess launches one hidden Go process and records it for
# teardown. Empty arguments are filtered because worker model flags are built
# from small hashtables where some workers intentionally advertise no models.
function Start-LogServeProcess {
  param([string[]]$Arguments)
  $filtered = $Arguments | Where-Object { $_ -ne $null -and $_ -ne "" }
  $proc = Start-Process `
    -FilePath "go" `
    -ArgumentList $filtered `
    -WorkingDirectory $root `
    -WindowStyle Hidden `
    -PassThru
  $script:procs += $proc
}

try {
  # Start the shared log first because both control and workers depend on it
  # for durable task and model state.
  Start-LogServeProcess -Arguments @(
    "run",
    "./cmd/logserve-logd",
    "--addr",
    $logAddr,
    "--data-dir",
    (Join-Path $dataDir "logstore")
  )
  Start-Sleep -Seconds 2

  Start-LogServeProcess -Arguments @(
    "run",
    "./cmd/logserve-control",
    "--addr",
    $controlAddr,
    "--log-addr",
    $logAddr
  )
  Start-Sleep -Seconds 2

  # The third worker has an empty model list to keep ordinary task capacity in
  # the same smoke while model-specific workers exercise LLM routing.
  foreach ($worker in @(
    @{ id = "worker-1"; models = "model-A:v1" },
    @{ id = "worker-2"; models = "model-B:v1" },
    @{ id = "worker-3"; models = "" }
  )) {
    Start-LogServeProcess -Arguments @(
      "run",
      "./cmd/logserve-worker",
      "--worker-id",
      $worker.id,
      "--control-addr",
      $controlAddr,
      "--log-addr",
      $logAddr,
      "--executor",
      (Join-Path $root "executor\python\server.py"),
      "--models",
      $worker.models,
      "--capacity",
      "1"
    )
  }
  Start-Sleep -Seconds 4

  python (Join-Path $root "examples\rag_llm\workflow.py")
}
finally {
  # Stop all processes started by this script before deleting the temporary CLI
  # binary that SDK helpers used during the example run.
  foreach ($proc in $procs) {
    if ($proc -and -not $proc.HasExited) {
      Stop-Process -Id $proc.Id -Force
    }
  }
  if (Test-Path -LiteralPath $cliPath) {
    Remove-Item -LiteralPath $cliPath -Force -ErrorAction SilentlyContinue
  }
}
