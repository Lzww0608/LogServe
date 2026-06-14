$ErrorActionPreference = "Stop"

$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-task-smoke-" + [guid]::NewGuid().ToString("N"))
$logAddr = "127.0.0.1:55051"
$controlAddr = "127.0.0.1:55052"

$env:GOCACHE = Join-Path $root ".gocache"
$env:PYTHONPATH = Join-Path $root "sdk\python"
$env:LOGSERVE_CONTROL_ADDR = $controlAddr

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
  python (Join-Path $root "examples\hello_task\add.py")
}
finally {
  if ($proc -and -not $proc.HasExited) {
    Stop-Process -Id $proc.Id -Force
  }
}
