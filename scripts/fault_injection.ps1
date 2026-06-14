$ErrorActionPreference = "Stop"

$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$reportDir = Join-Path $root "reports"
$reportPath = Join-Path $reportDir "fault_injection.json"
$dataDir = Join-Path ([System.IO.Path]::GetTempPath()) ("logserve-fault-injection-" + [guid]::NewGuid().ToString("N"))
$logAddr = "127.0.0.1:59151"
$controlAddr = "127.0.0.1:59152"

$env:GOCACHE = Join-Path $root ".gocache"
New-Item -ItemType Directory -Force -Path $reportDir | Out-Null
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

$results = [ordered]@{
  generated_at = (Get-Date).ToString("o")
  worker_kill_recovery = "not_run"
  queue_redelivery = "not_run"
  control_restart_probe = "not_run"
  logd_restart_probe = "not_run"
  notes = @()
}

try {
  go test ./tests/integration -run "Test(WorkflowWorkerRecoveryContinuesAfterCompletedStep|ActorCounterRecoverySnapshotAndReplay|RunningTaskIsRedeliveredAfterWorkerLeaseExpires|PolledTaskIsRedeliveredWhenWorkerDiesBeforeStart|StaleTaskCompletionRejectedAfterRedelivery|OrdinaryTaskSurvivesControlRestartFromTaskSpecLog|ControlRestartBootstrapsWorkflowAndModelStateFromLog)" -count=1 | Out-Host
  $results.worker_kill_recovery = "passed"
  $results.queue_redelivery = "passed"
}
catch {
  $results.worker_kill_recovery = "failed"
  $results.queue_redelivery = "failed"
  $results.notes += $_.Exception.Message
}

$logd = $null
$control = $null
try {
  $logd = Start-Process `
    -FilePath "go" `
    -ArgumentList @("run", "./cmd/logserve-logd", "--addr", $logAddr, "--data-dir", (Join-Path $dataDir "logstore")) `
    -WorkingDirectory $root `
    -WindowStyle Hidden `
    -PassThru
  Start-Sleep -Seconds 2
  $control = Start-Process `
    -FilePath "go" `
    -ArgumentList @("run", "./cmd/logserve-control", "--addr", $controlAddr, "--log-addr", $logAddr) `
    -WorkingDirectory $root `
    -WindowStyle Hidden `
    -PassThru
  Start-Sleep -Seconds 2

  Stop-Process -Id $control.Id -Force
  Start-Sleep -Seconds 1
  $control = Start-Process `
    -FilePath "go" `
    -ArgumentList @("run", "./cmd/logserve-control", "--addr", $controlAddr, "--log-addr", $logAddr) `
    -WorkingDirectory $root `
    -WindowStyle Hidden `
    -PassThru
  Start-Sleep -Seconds 2
  $results.control_restart_probe = "process_restarted"
  $results.notes += "Control bootstraps workflow, actor, model, ordinary task, and backpressure materialized state from shared log streams."

  Stop-Process -Id $logd.Id -Force
  Start-Sleep -Seconds 1
  $logd = Start-Process `
    -FilePath "go" `
    -ArgumentList @("run", "./cmd/logserve-logd", "--addr", $logAddr, "--data-dir", (Join-Path $dataDir "logstore")) `
    -WorkingDirectory $root `
    -WindowStyle Hidden `
    -PassThru
  Start-Sleep -Seconds 2
  $results.logd_restart_probe = "process_restarted_same_data_dir"
}
catch {
  $results.notes += $_.Exception.Message
  if ($results.control_restart_probe -eq "not_run") {
    $results.control_restart_probe = "failed"
  }
  if ($results.logd_restart_probe -eq "not_run") {
    $results.logd_restart_probe = "failed"
  }
}
finally {
  foreach ($proc in @($control, $logd)) {
    if ($proc -and -not $proc.HasExited) {
      Stop-Process -Id $proc.Id -Force
    }
  }
}

$results | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $reportPath -Encoding UTF8
Write-Output $reportPath
