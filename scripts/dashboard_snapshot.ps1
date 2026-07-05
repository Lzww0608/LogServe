$ErrorActionPreference = "Stop"

# Writes dashboard/snapshot.json from an existing control plane when possible,
# or starts a short-lived local logd/control pair as a fallback. The snapshot is
# used as a checked-in dashboard fixture, so no worker process is required.
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

# Write-DashboardSnapshot invokes logservectl and persists stdout only when the
# command succeeds. It temporarily relaxes ErrorActionPreference so stderr can be
# captured and surfaced if the caller needs to fall back to a local runtime.
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

# Start-LogServeProcess launches one hidden Go process and records the handle so
# the fallback runtime can be torn down even when snapshot generation fails.
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
    # If no compatible control plane is already running, start the smallest
    # runtime needed for dashboard-snapshot to return a structurally valid view.
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
  # Only processes created by this script are tracked, so stopping them cannot
  # terminate a caller-provided control plane used by the first snapshot attempt.
  foreach ($proc in $procs) {
    if ($proc -and -not $proc.HasExited) {
      Stop-Process -Id $proc.Id -Force
    }
  }
}
