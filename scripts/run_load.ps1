# Run all six load shapes against the stand-in, appending to one results.csv.
# Each scenario that needs different limits restarts the stand-in with its own
# config. Run from the project root:
#   powershell -ExecutionPolicy Bypass -File scripts\run_load.ps1
#
# The client reserves provider capacity with its admission bucket. This tests
# the designed queueing/shedding policy against the stand-in's service-side
# limits; the pre-hardening no-admission baseline is retained in
# results/archive_pre_hardening.
$ErrorActionPreference = "Continue"
Set-Location (Join-Path $PSScriptRoot "..")
$bin = ".\bin"
$log = "results\load_run.log"
# A long real sweep may already be holding bin\relay.exe open, so the load run
# can use a freshly built copy instead of fighting the file lock.
$relay = if ($env:RELAY_EXE) { $env:RELAY_EXE } else { "$bin\relay.exe" }

function Start-StandIn($config, [string[]]$extra = @()) {
    $a = @("-addr", "127.0.0.1:8099", "-config", $config) + $extra
    $p = Start-Process -FilePath "$bin\standin.exe" `
        -ArgumentList $a -PassThru -WindowStyle Hidden
    Start-Sleep -Seconds 2
    Write-Host ("stand-in up: {0} {1} (pid {2})" -f $config, ($extra -join " "), $p.Id)
    return $p
}

function Stop-StandIn($p) {
    if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force }
    Start-Sleep -Milliseconds 700
}

function Run-Scenario($name, $extra) {
    Write-Host ("=== scenario {0} ===" -f $name)
    $args = @("load", "-source", "standin",
              "-base", "http://127.0.0.1:8099/openai/v1",
              "-scenario", $name, "-setting", "p220_k6_ryes", "-limits",
              "-out", "results\load_results.csv", "-append",
              "-detail", "results\load_detail.jsonl") + $extra
    & $relay @args 2>&1 | Tee-Object -FilePath $log -Append | Write-Host
}

$out = "results\load_results.csv"
Remove-Item $out -ErrorAction SilentlyContinue
Remove-Item "results\load_detail.jsonl" -ErrorAction SilentlyContinue
Remove-Item $log -ErrorAction SilentlyContinue
# scenario_meta.json is rebuilt from these six runs; stale entries would show
# a scenario that this run did not measure.
Remove-Item "results\scenario_meta.json" -ErrorAction SilentlyContinue

# default limits (rpm 30, tpm 8000, rpd 1000)
$p = Start-StandIn "standin.json"
try {
    Run-Scenario "sustainable" @()
    Run-Scenario "steady" @()
    Run-Scenario "burst" @()
    Run-Scenario "overload" @()
} finally { Stop-StandIn $p }

# per-minute limit halved: stand-in RPM=15
$p = Start-StandIn "standin_rpm15.json"
try {
    Run-Scenario "rpm_half" @("-rpm", "15")
} finally { Stop-StandIn $p }

# daily budget exhausted: both the service and the client mirror are scaled to
# an 8,000-token day so the boundary is reachable in 45 seconds. One whole
# request (6,286 tokens reserved) fits and the second does not, so the shape
# shows one served answer and then the quota reply. The client reserves a whole
# request, so it refuses a call the service might still have served on its
# smaller actual usage — deliberately: a call that starts and then gets a 429
# costs more than an immediate honest refusal.
$p = Start-StandIn "standin.json" @("-tpd", "8000")
try {
    Run-Scenario "daily_exhausted" @("-tpd", "8000")
} finally { Stop-StandIn $p }

Write-Host "wrote $out"
