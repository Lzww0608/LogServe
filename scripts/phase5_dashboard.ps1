$ErrorActionPreference = "Stop"

$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$dashboardDir = Join-Path $root "dashboard"
$snapshotPath = Join-Path $dashboardDir "snapshot.json"

$env:GOCACHE = Join-Path $root ".gocache"
if (-not $env:LOGSERVE_CONTROL_ADDR) {
  $env:LOGSERVE_CONTROL_ADDR = "127.0.0.1:50052"
}

$procs = @()
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-dashboard-" + [guid]::NewGuid().ToString("N"))
$script:lastSnapshotError = ""

function Write-DashboardSnapshot {
  $oldErrorActionPreference = $ErrorActionPreference
  $ErrorActionPreference = "Continue"
  try {
    $output = go run ./cmd/logservectl dashboard-snapshot 2>&1
    $exitCode = $LASTEXITCODE
  }
  finally {
    $ErrorActionPreference = $oldErrorActionPreference
  }
  if ($exitCode -ne 0) {
    $script:lastSnapshotError = ($output | Out-String)
    return $false
  }
  $output | Set-Content -LiteralPath $snapshotPath -Encoding UTF8
  return $true
}

function Start-LogServeProcess {
  param([string[]]$Arguments)
  $proc = Start-Process `
    -FilePath "go" `
    -ArgumentList $Arguments `
    -WorkingDirectory $root `
    -WindowStyle Hidden `
    -PassThru
  $script:procs += $proc
}

try {
  if (-not (Write-DashboardSnapshot)) {
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    $logAddr = "127.0.0.1:59251"
    $controlAddr = "127.0.0.1:59252"
    $env:LOGSERVE_CONTROL_ADDR = $controlAddr
    Start-LogServeProcess -Arguments @("run", "./cmd/logserve-logd", "--addr", $logAddr, "--data-dir", (Join-Path $dataDir "logstore"))
    Start-Sleep -Seconds 2
    Start-LogServeProcess -Arguments @("run", "./cmd/logserve-control", "--addr", $controlAddr, "--log-addr", $logAddr)
    Start-Sleep -Seconds 2
    if (-not (Write-DashboardSnapshot)) {
      throw $script:lastSnapshotError
    }
  }
  Write-Output $snapshotPath
}
finally {
  foreach ($proc in $procs) {
    if ($proc -and -not $proc.HasExited) {
      Stop-Process -Id $proc.Id -Force
    }
  }
}
