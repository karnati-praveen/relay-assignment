# Finish the submission from the measured data, in one deterministic run.
#
# Refuses to publish anything until all 20 real answers exist AND every one has
# a hand verdict in results/quality_manual.csv. The Table 3 quality columns are
# hand-verified facts, not the citation-only automatic grade.
#
# Prose lives in report.template.md; report.md is an output of step 5. Every step
# is re-runnable: finalize_report.py patches from the template, so a failure
# after it has written report.md does not poison the next attempt.
#
# Usage (from the project root):
#   powershell -ExecutionPolicy Bypass -File scripts\finalize.ps1
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
$env:PYTHONIOENCODING = "utf-8"

function Fail($msg) { Write-Host "FAIL: $msg" -ForegroundColor Red; exit 1 }
function Ok($msg)   { Write-Host "ok: $msg" -ForegroundColor Green }

# --- 1. the sweep must be complete ------------------------------------------
$n = python -c "import json;print(sum(1 for l in open('results/quality_real.detail.jsonl',encoding='utf-8') if l.strip()))"
if ([int]$n -lt 20) {
    Write-Host "sweep incomplete: $n/20 questions measured." -ForegroundColor Yellow
    Write-Host "The background run appends one JSON line per answer; leave it running."
    exit 1
}
Ok "sweep complete: $n rows"

# --- 2. every answer must carry a hand verdict ------------------------------
if (-not (Test-Path "results\quality_manual.csv")) {
    Fail "missing results/quality_manual.csv. Grade the answers first: python scripts/review_quality.py"
}

# --- 3. rebuild the authoritative results.csv ------------------------------
python scripts\build_results.py
if ($LASTEXITCODE -ne 0) { Fail "build_results.py" }
Ok "results.csv rebuilt"

# --- 4. every required table, from the raw rows ----------------------------
python scripts\analyze.py | Out-Null
if ($LASTEXITCODE -ne 0) { Fail "analyze.py" }
Ok "results/tables.md rebuilt"

# --- 5. patch report.md from the same numbers ------------------------------
python scripts\finalize_report.py
if ($LASTEXITCODE -ne 0) { Fail "finalize_report.py (report.md untouched)" }
Ok "report.md patched, quality_manual.md regenerated"

# No placeholder may survive: a TBD in the PDF is an automatic credibility loss.
$tbd = Select-String -Path "report.md" -Pattern "TBD"
if ($tbd) {
    $tbd | ForEach-Object { Write-Host ("  {0}: {1}" -f $_.LineNumber, $_.Line.Trim()) }
    Fail "report.md still has TBD placeholders"
}

# --- 6. render + verify -----------------------------------------------------
$pdfPages = python scripts\build_report_pdf.py
if ($LASTEXITCODE -ne 0) { Fail "build_report_pdf.py" }
if ($pdfPages -notmatch "pages:\s*2\b") { Fail "report.pdf is not two pages: $pdfPages" }
Ok "report.pdf fits two pages ($pdfPages)"

# Join first: check_columns.py prints several lines, and -notmatch on an array
# filters it, so a multi-line pass would still look like a failure here.
$cols = (python scripts\check_columns.py) -join "`n"
if ($LASTEXITCODE -ne 0 -or $cols -notmatch "RESULTS.CSV COLUMN MATCH: True") {
    Fail "results.csv columns: $cols"
}
Ok "results.csv has the brief's 12 columns"

$rows = (Get-Content results\results.csv | Measure-Object -Line).Lines - 1
Write-Host ""
Write-Host "READY: results.csv has $rows rows (real quality sweep + load scenarios)." -ForegroundColor Cyan
Write-Host "Next: powershell -ExecutionPolicy Bypass -File scripts\stage_submission.ps1"
