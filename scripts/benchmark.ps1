$ErrorActionPreference = "Stop"

# Runs a disposable local LogServe benchmark stack and writes the latest JSON
# report under benchmarks/. The script builds logservectl into the temporary
# data directory so the benchmark can point SDK helpers at an isolated CLI.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-benchmark-" + [guid]::NewGuid().ToString("N"))
$reportDir = Join-Path $root "benchmarks"
$reportPath = Join-Path $reportDir "benchmark_latest.json"
$cliPath = Join-Path $dataDir "logservectl.exe"
$logAddr = "127.0.0.1:59051"
$controlAddr = "127.0.0.1:59052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr
$env:LOGSERVE_CLI = $cliPath

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
New-Item -ItemType Directory -Force -Path $reportDir | Out-Null
go build -o $cliPath ./cmd/logservectl

$procs = @()

# Start-LogServeProcess launches one Go-based LogServe process in the
# repository root and records the process handle for best-effort cleanup in the
# finally block. Empty arguments are stripped so optional flags can be composed
# without changing Start-Process semantics.
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
  # The benchmark uses fixed loopback ports inside a unique data directory, so
  # reruns do not share logstore state even when the previous report is kept.
  Start-LogServeProcess -Arguments @("run", "./cmd/logserve-logd", "--addr", $logAddr, "--data-dir", (Join-Path $dataDir "logstore"))
  Start-Sleep -Seconds 2
  Start-LogServeProcess -Arguments @("run", "./cmd/logserve-control", "--addr", $controlAddr, "--log-addr", $logAddr)
  Start-Sleep -Seconds 2

  foreach ($worker in @(
    @{ id = "bench-worker-1"; models = "model-A:v1" },
    @{ id = "bench-worker-2"; models = "model-B:v1" },
    @{ id = "bench-worker-3"; models = "" }
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

  # Capture stdout as the report body; a non-zero Python exit means the report
  # may be partial and must not replace benchmark_latest.json silently.
  $benchmarkOutput = python (Join-Path $root "examples\evaluation\benchmark.py")
  if ($LASTEXITCODE -ne 0) {
    throw "benchmark failed"
  }
  $benchmarkOutput | Set-Content -LiteralPath $reportPath -Encoding UTF8
  Write-Output $reportPath
}
finally {
  # Processes are started out-of-process rather than as jobs, so each handle
  # must be stopped explicitly to avoid leaking benchmark daemons.
  foreach ($proc in $procs) {
    if ($proc -and -not $proc.HasExited) {
      Stop-Process -Id $proc.Id -Force
    }
  }
  if (Test-Path -LiteralPath $cliPath) {
    Remove-Item -LiteralPath $cliPath -Force -ErrorAction SilentlyContinue
  }
}
