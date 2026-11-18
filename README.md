# The Relay

A paper-question-answering assistant with a five-step LLM pipeline (understand -> search -> decide -> draft -> check), hybrid BM25 retrieval with re-ranking, admission control, and reproducible load measurements against a stand-in model service. See `report.md` for the full engineering write-up and `results/` for the raw measurements (663 requests, one row per request).

## Quick start (three commands)

```bash
# 1. ask one question of the real service (papers/text ships, so no fetch step)
GROQ_API_KEY=... go run ./cmd/relay ask -q "Why divide by sqrt(d_k)?"

# 2. terminal 1: start the stand-in; terminal 2: run one load shape against it
go run ./cmd/standin -addr 127.0.0.1:8099 -config standin.json
go run ./cmd/relay load -source standin -scenario steady -setting p220_k6_ryes \
    -limits -out results/steady_check.csv -detail results/steady_check.jsonl

# 3. recompute the tables from results/results.csv and the shipped raw files
python scripts/analyze.py
```

On Windows the binaries are `bin\relay.exe` / `bin\standin.exe`, and the key is
set for the session first: `$env:GROQ_API_KEY='...'; go run ./cmd/relay ask -q
"..."`. `GROQ_API_KEY` is read when `.groq_key` is absent; no other setup is
needed because `papers/text/` holds the extracted text the retriever reads.

## Layout

```
cmd/relay        the pipeline CLI (ask, probe, retrieval, load, inspect, ...)
cmd/standin      the stand-in model service
internal/
  provider/      model-provider client (real Groq + stand-in), streaming SSE
  papers/        loads the three papers and chunks them on sentence boundaries
  retrieval/     BM25 index, re-ranker, piece-size settings
  pipeline/      the five-step pipeline, retries, admission control, traces
  limits/        token/request admission-control bucket
  eval/          question grading and the results.csv writer
data/questions.json   the 20 graded questions
papers/               the three assigned papers (extracted text; PDFs by script)
scripts/              calibration, analysis and run drivers
results/              raw measurements, results.csv, tables
```

`results/results.csv` is the one table the brief asks for: every request from
both sources, 663 rows, the twelve required columns, one row per request. The
other CSVs beside it are the per-run raw files it was built from (one file per
scenario or sweep), kept so any row in `results.csv` can be traced back to the
run that produced it.

## Papers

Fetched at the exact required versions:

- Attention Is All You Need, arXiv 1706.03762, **v7**
- Moshi, arXiv 2410.00037, **v1**
- Dynamic Chunking for End-to-End Hierarchical Sequence Modeling, arXiv
  2507.07955, **v2**

`papers/text/*.txt` are the extracted texts used by the retriever, and they ship,
so every command below runs offline. The PDFs themselves are left out of the
archive to keep it small; `python scripts/fetch_papers.py` downloads those exact
versioned files into `papers/` for anyone checking a cited page by eye.

## How it works

Five steps: `understand` (planner model → query + paper) → `search_library`
(**tool call**: schema `searchTool`, arguments validated and executed by
`invokeTool`) → `decide` (**branch**: refuse on weak coverage before drafting) →
`draft` (streamed; every claim carries `[paper p.N]`) → `check` (**loop**:
verify against the full passages, one revision).

The checker also validates every citation against the passages it was given
*and* against the planner's paper scope. A question that splices one paper's
fact onto another — e.g. asking for Moshi's WMT 2014 BLEU score, which belongs
to Attention — is refused when the draft cites the other paper, rather than
answered from the wrong one (`TestValidCitationSetRejectsWrongPaperAndFabricatedPage`).

## Commands

```bash
go build ./...

# one question against the real service (needs .groq_key or GROQ_API_KEY)
./bin/relay.exe ask -q "What is the dimension of the model in the base Transformer?"

# one question against the stand-in
./bin/standin.exe -addr 127.0.0.1:8099 -config standin.json &
./bin/relay.exe ask -source stand-in -base http://127.0.0.1:8099/openai/v1 -q "..."

# measurements
./bin/relay.exe probe -n 10 -out results/real_probe.jsonl
./bin/relay.exe retrieval -source real -settings p220_k6_ryes -out results/quality_real.csv
./bin/relay.exe recall -out results/retrieval_recall.csv   # no model calls
powershell -File scripts/run_load.ps1
python scripts/analyze.py
```

`recall` measures the retrieval step alone: for each answerable question it
checks whether the context a setting returns contains the hand-written gold
sentence (`gold` in `data/questions.json`), with the paper routing held at the
question's own paper. It makes no model call, so all five settings can be
compared without spending the daily token budget.

`ask` enables admission control by default (`-limits=false` for a raw call). The
bucket reserves **one whole request** — planner, draft passages and check — in
the next minute that fits, then settles the reservation against actual usage.
It refuses in milliseconds (internally `busy`, written to `results.csv` as
`timeout`, the brief's nearest status) rather than parking a caller beyond
8 s. `retrieval` deliberately runs without the bucket: it is a measurement
harness for the provider's own 429 behaviour, not the serving path.

A full sweep of the real service has to live inside one 200,000-token day that
refills at ~2.31 tokens/s, so `retrieval` paces itself: `-budgetwait` seconds
per retry and `-tpd` (the provider's allowance) let it sleep long enough to earn
a whole request before asking. Waiting only for the provider's `retry-after` is
not enough — the retry restarts the request and re-spends the planner and draft
tokens the failed attempt already paid for, so the sweep diverges instead of
converging (`results/real_findings.md`, `cmd/relay/budget_test.go`). Every
question is appended to the detail file as it completes, so the sweep is
resumable with `-resume`. A question that ends with zero tokens spent and a
transport error is retried inside the harness rather than recorded, so a DNS
blip cannot consume its only slot.

## Secrets

The Groq API key lives in `.groq_key`, which is git-ignored and is **not** part
of the submission; if the file is absent the CLI reads `GROQ_API_KEY` instead.
The report's conversation history has it redacted.

## Submission

Submit one folder (set to "Anyone with the link can view") containing:

- `report.md` and `report.pdf` (the 2-page write-up, required tables included;
  the same tables are also in `results/tables.md`),
- the code (this repository) and `papers/text/` at the three pinned versions, so
  `cmd/relay` runs on a new question with no fetch step, and
- `results/results.csv` (one row per request, the exact required columns).

`results.csv` holds **both** sources. The `source` column is the brief's literal
`real` or `stand-in`; `scenario` tells them apart:

- **40 rows** with `source=real`, `scenario=quality_sweep` — the real-service
  quality runs: 20 questions at the chosen `p220_k6_ryes`, and 10 each at
  `p120_k4_rno` and `p450_k3_ryes` (the same first ten questions). These carry
  the **hand-verified** `answer_grade`, so Table 3 recomputes from this file.
- **20 rows** with `source=real`, `scenario=quality_fix20` — the post-hoc
  in-sample repair run (`-variant fixed -draftmaxtokens 1200`), reported
  separately in prose and never merged into the baseline.
- **100 rows** with `source=stand-in`, `scenario=retrieval_standin` — the five
  retrieval settings (20 questions each) on the stand-in, backing Table 3's
  stand-in cost and latency rows (`answer_grade=not measured`).
- **503 rows across the six load shapes** with `source=stand-in` —
  `sustainable`, `steady`, `burst`, `overload`, `rpm_half`, `daily_exhausted`,
  one row per request. `sustainable` offers one request a minute, which is what
  the 8,000-token minute actually buys, so it is the only shape whose answers
  are not queued. Their `answer_grade` is `not measured`: the stand-in's draft
  is canned text, so grading it would invent quality.

`status` is exactly the brief's four values: `ok`, `rate_limited` (the
provider's own 429, including the daily budget), `timeout` (including a request
our admission control gave up on before any provider call — the prose
distinguishes it from a mid-flight HTTP timeout), or `error`. Tables 2a/2b are computed from the load rows only; the
real rows feed Table 3 and the quality figures. `scripts/analyze.py`
regenerates every table from this one file.

Supporting raw material is under `results/`: `load_results.csv` (the load rows
before the merge), `load_detail.jsonl` (one JSON line per load request with the
waiting/working/retrying split §4 quotes), `retrieval_standin.csv` (the five
settings on the stand-in), `retrieval_recall.csv` (offline gold-sentence recall
per setting), `quality_real.detail.jsonl` / `quality_p120.detail.jsonl` /
`quality_p450.detail.jsonl` (one JSON line per real answer, appended as it
completes), `quality_manual.csv` and `quality_manual_p120.csv` /
`quality_manual_p450.csv` (my hand verdict per answer, and the
`quality_manual.md` rendered from the chosen one), `real_findings.md`,
`scenario_meta.json`, `conversation_history.md`, `archive_checkbug/` — the
two answers the first sweep refused because its checker read only the first 200
characters of each passage (§"The checker bug that invalidated the first sweep"
in `measurement_plan.md`) — and `archive_pre_checkerfix/`, the five-setting
stand-in run as it was measured while that checker was still in place (the
"before" side of the re-measurement).

## Finishing the measurements

The real quality sweep is resumable and spends a shared daily token bucket:

```
./bin/relay.exe retrieval -source real -settings p220_k6_ryes \
    -out results/quality_real.csv -budgetwait 300 -resume
```

Two comparison settings then run on the same first ten questions (Table 3 needs
three settings with measured quality; a full 20-question sweep per setting does
not fit the remaining token days):

```
powershell -ExecutionPolicy Bypass -File scripts\run_extra_sweeps.ps1
```

When all answers exist, grade them by reading each answer against its cited
page (`python scripts/review_quality.py`, one row per question in
`results/quality_manual.csv`; for a comparison run use
`python scripts/review_quality.py results/quality_p120.detail.jsonl --manual
results/quality_manual_p120.csv`), then:

```
powershell -ExecutionPolicy Bypass -File scripts\finalize.ps1
```

That rebuilds `results.csv`, `results/tables.md`, `quality_manual.md` and
`report.pdf`, and refuses to publish if the sweep is incomplete, a verdict is
missing, or the prose no longer fits two pages. `scripts/sweep_stats.py` is the
single definition of the sweep's numbers, so `tables.md` and the report cannot
disagree.
