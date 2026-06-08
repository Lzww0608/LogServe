$ErrorActionPreference = "Stop"

$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-phase3-" + [guid]::NewGuid().ToString("N"))
$cliPath = Join-Path $dataDir "logservectl.exe"
$logAddr = "127.0.0.1:57051"
$controlAddr = "127.0.0.1:57052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr
$env:LOGSERVE_CLI = $cliPath

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
go build -o $cliPath ./cmd/logservectl

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
  Start-Sleep -Seconds 4
  python (Join-Path $root "examples\actor_counter\counter.py")
}
finally {
  if ($proc -and -not $proc.HasExited) {
    Stop-Process -Id $proc.Id -Force
  }
  if (Test-Path -LiteralPath $cliPath) {
    Remove-Item -LiteralPath $cliPath -Force -ErrorAction SilentlyContinue
  }
}
