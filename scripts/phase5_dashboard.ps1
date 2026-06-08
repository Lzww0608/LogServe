$ErrorActionPreference = "Stop"

$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dashboardDir = Join-Path $root "dashboard"
$snapshotPath = Join-Path $dashboardDir "snapshot.json"

$env:GOCACHE = Join-Path $root ".gocache"
if (-not $env:LOGSERVE_CONTROL_ADDR) {
  $env:LOGSERVE_CONTROL_ADDR = "127.0.0.1:50052"
}

go run ./cmd/logservectl dashboard-snapshot | Set-Content -LiteralPath $snapshotPath -Encoding UTF8
Write-Output $snapshotPath
