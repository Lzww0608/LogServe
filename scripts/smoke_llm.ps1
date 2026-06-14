$ErrorActionPreference = "Stop"

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
  foreach ($proc in $procs) {
    if ($proc -and -not $proc.HasExited) {
      Stop-Process -Id $proc.Id -Force
    }
  }
  if (Test-Path -LiteralPath $cliPath) {
    Remove-Item -LiteralPath $cliPath -Force -ErrorAction SilentlyContinue
  }
}
