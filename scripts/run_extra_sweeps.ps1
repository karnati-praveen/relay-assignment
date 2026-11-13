# Run the two comparison settings for Table 3 after the chosen sweep finishes.
#
# The provider's daily token budget is one bucket shared by every model call, so
# the sweeps must run one after another: running them in parallel does not make
# the refill faster, it only turns a paced sweep into a 429.
#
# Each comparison setting asks the first ten questions (data/questions10.json).
# That is the same subset as the first half of the chosen sweep, and it is half
# the token cost, because a 20-question sweep per setting does not fit in the
# time left on a 200,000-token day.
#
# Usage (from the project root), while the chosen sweep is still running:
#   powershell -ExecutionPolicy Bypass -File scripts\run_extra_sweeps.ps1
$ErrorActionPreference = "Continue"
Set-Location (Join-Path $PSScriptRoot "..")
$log = "results\extra_sweeps.log"
$detail = "results\quality_real.detail.jsonl"
# The chosen sweep holds bin\relay.exe open for hours, so the comparison runs
# use a freshly built copy when one is given; the copy also carries the
# reservation measured on the real answers (6,286 tokens), which is what the
# pacing waits against.
$relay = if ($env:RELAY_EXE) { $env:RELAY_EXE } else { ".\bin\relay.exe" }

function Log($msg) {
    $line = "{0}  {1}" -f (Get-Date -Format "HH:mm:ss"), $msg
    Write-Host $line
    Add-Content -Path $log -Value $line
}

Remove-Item $log -ErrorAction SilentlyContinue
Log "waiting for the chosen sweep to reach 20 answers"

while ($true) {
    $n = 0
    if (Test-Path $detail) {
        $n = @(Get-Content $detail | Where-Object { $_.Trim() -ne "" }).Count
    }
    if ($n -ge 20) { break }
    Start-Sleep -Seconds 120
}
Log "chosen sweep complete ($n rows); starting p120_k4_rno on q01-q10"

& $relay retrieval -source real -settings p120_k4_rno `
    -questions data\questions10.json -out results\quality_p120.csv `
    -budgetwait 300 -budgetmax 80 -tpd 200000 -resume 2>&1 |
    Tee-Object -FilePath $log -Append
Log "p120_k4_rno done (exit $LASTEXITCODE)"

& $relay retrieval -source real -settings p450_k3_ryes `
    -questions data\questions10.json -out results\quality_p450.csv `
    -budgetwait 300 -budgetmax 80 -tpd 200000 -resume 2>&1 |
    Tee-Object -FilePath $log -Append
Log "p450_k3_ryes done (exit $LASTEXITCODE)"
