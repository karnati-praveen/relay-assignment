# Measurement plan and honesty notes

## What each source is good for

| | real service | stand-in |
|---|---|---|
| Latency distribution | yes (small n) | yes (large n, controlled) |
| Rate-limit behaviour | yes (RPM, TPM) | yes (RPM, TPM, RPD, TPD) |
| Answer quality | **only real** | no (canned answers) |
| Retrieval quality (gold sentence in context) | no (costs the budget) | yes, offline (same text) |
| High load | no (daily budget) | yes |

**The stand-in cannot be used for answer quality.** It returns a fixed,
plausible-looking cited answer, so every answerable question "passes" a purely
automatic grader. Any quality number in this submission comes from the real
service and was verified by hand.

## The automatic grader is deliberately coarse

`answer_grade` in results.csv has four allowed values and records only what can
be checked without ground truth:

- `correct` = answered, with at least one citation
- `wrong` = answered without a citation, or answered a question the papers do
  not answer (a hallucination)
- `refused_correctly` = refused a question the papers genuinely do not answer
- `refused_wrongly` = refused a question the papers do answer

It does **not** verify the fact. I verified the facts by hand; that verdict is
in `results/quality_manual.md` and is what the write-up quotes. In particular an
answer can be `correct` by this scheme while citing a page other than the one
the fact is stated on; page precision is reported separately.

## The checker bug that invalidated the first sweep

The first real 20-question sweep was thrown away, and the reason is worth
recording because it is a bug in the *measuring* side of the system.

`check()` built its prompt with `truncate(h.Chunk.Text, 200)`: the drafter saw
whole passages (1,300–1,700 characters), the checker saw the first 200
characters of each. On q02 (attention heads and head dimension) the supporting
sentence — `In this work we employ h = 8 parallel attention layers … dk = dv =
dmodel/h = 64` — sits at about character 900 of the 1,556-character attention
p.5 chunk. The checker was therefore asked to confirm a claim against a passage
that cut off 700 characters before the sentence supporting it, answered
`ok=false`, the pipeline re-drafted, and the second check failed the same way.
The row was written as `status=error`, 3,711 ms, 6,120 in + 1,381 out, and the
automatic grade called a correct answer `refused_wrongly`.

Found by reading the first two answers by hand before trusting the sweep; then
`relay debug-retrieve -full` printed the passage and the offset was obvious.

What changed:

- `checkPrompt()` quotes every passage in full (cap 4,000 characters — a guard
  against a pathological chunk, never reached by a real one), so the checker
  sees exactly what the drafter saw.
- `TestCheckPromptCarriesFullPassage` fails if any passage is truncated.
- The two bad rows are kept verbatim in
  `results/archive_checkbug/quality_real.checkbug.detail.jsonl`; they are not
  part of any published number.
- The sweep was restarted from question 1 rather than topped up, because mixing
  rows from two different checkers into one accuracy figure would be a lie even
  if every individual row were honest.

Cost of the fix, stated plainly: verification now carries the passages, so one
request averages ~6,364 tokens — q01, the one measured end to end, was 6,123
(80 understand, 2,526 draft (2,049 prompt + 477 generated), 3,517 check) —
instead of ~2,900, and the end-to-end throughput gain over the original
6,386-token design is **1.0×** rather than the 2.2× measured while the checker
could not see. The chunker halved the draft prompt and the honest checker spent
the whole saving on passages it now actually reads. I report the smaller number,
and the reservation was resized to match (`requestEstimate` now returns 6,286,
pinned by `TestRequestEstimateCoversAMeasuredRequest`).

## The stand-in's cost was re-measured after the checker fix

`results/retrieval_standin.csv` (the five settings on the stand-in, which feed
Table 3's stand-in rows and the §3 token cross-check) was first produced while
the checker read 200-character excerpts: 3,604 tokens/answer at p220_k6. After
`checkPrompt()` started quoting whole passages, the same 100 requests cost
5,626, because the check prompt is now as large as the draft's. The run was
redone against `standin_nolimit.json` rather than left as a smaller number that
no longer described the code; the old file is kept verbatim in
`results/archive_pre_checkerfix/`. The stand-in still understates q01's real
6,123 by ~8%, and that gap is its missing reasoning tokens.

## Why one full quality sweep and two ten-question subsets

The free tier allows 1,000 requests/day and 200,000 tokens/day, and the bucket
refills at ~2.3 tokens/s, so the sustainable rate is ~8,300 tokens/hour — one
answer every ~44 minutes at ~6,100 tokens. Five 20-question sweeps are therefore
impossible in one day. I spent the real budget on the chosen setting (all 20
questions) and measured the two comparison settings on the same first ten
questions (q01–q10): that is one measured quality percentage per setting on an
identical subset, at half the token cost. The remaining settings were measured
on the stand-in for latency and token cost only, and with `relay recall` — which
makes no model call — for retrieval quality. Recall asks whether the context a
setting returns contains the hand-written sentence that answers the question;
that is a property of retrieval, so it is valid on either source and costs
nothing.

## Why the real sweep takes most of a day

The daily bucket refills at about 2.3 tokens/s and one request costs ~6,364
tokens, so a 20-question sweep is roughly 20 waits of ~45 minutes: 14–15 hours of
wall time. A question that needs a revision spends ~12,000 tokens instead, so
the real figure can be longer. The run is resumable (`-resume` skips
completed questions) and appends each answer to `quality_real.detail.jsonl`
before moving on, so a restart costs nothing. The chosen setting is deliberately
not shortened: 20 questions is the whole set, and a
budget-stopped question is labelled `not_measured` rather than being counted as a
refusal or a guess.

The wait has to be for a *whole request* (`-budgetwait` plus `-tpd`), not for the
provider's `retry-after`. The first attempt proved that the hard way: retry-after
funds only the call that was refused, the retry restarts from the planner, and
each cycle therefore ended with less headroom than it started with — six waits,
47 minutes, zero answers (`quality_real_run.attempt1.log`). Pacing by the refill
rate instead means the limit is usually never hit.

## Two grades, and why Table 3 uses the second

The detail files record the pipeline's own `grade_auto`, which only knows
"answered with a citation" from "refused". It cannot tell a right fact from a
wrong one. Every real answer is therefore also read by me against the page it
cited, and the verdict is recorded in `results/quality_manual*.csv`; Table 3 and
the report quote that hand verdict. `results.csv` carries the hand verdict on
every real row, so the table recomputes from the raw file. `scripts/sweep_stats.py`
holds both numbers, and `scripts/finalize_report.py` refuses to patch `report.md`
until every question of every real run has a hand verdict — an ungraded sweep
cannot be published as accuracy.

## Reproducing the load tables

The load shapes are the one part a reviewer can re-run without spending a token,
and the admission counts should reproduce even where the millisecond figures do
not. One shape on its own, in five minutes, on the binary that ships:

```
.\bin\standin.exe -addr 127.0.0.1:8099 -config standin.json
.\bin\relay load -source standin -base http://127.0.0.1:8099/openai/v1 `
    -scenario steady -setting p220_k6_ryes -limits `
    -out results\steady_check.csv -detail results\steady_check.jsonl
```

(or `scripts/run_load.ps1` for all six, which starts and stops the stand-in for
each shape). Measured again with that binary and the current 6,286-token
reservation: **101 requests, 6 answered, 95 shed, peak queue 2** — the same
counts as Table 2a, against 8,000 TPM and an 8 s admission wait. The percentiles
move by a few hundred milliseconds between runs because the stand-in samples its
latency per call; the counts do not, because they follow from the reservation and
the offered rate.

## The stand-in's slowdown, measured on its own

Table 2a cannot show the stand-in getting slower under load, because the
pipeline's admission gate keeps concurrency at one: above ~1.3 requests/minute
the budget admits one 6,286-token request per 47 s and sheds the rest, so the
load shows up as refusals, not as a latency tail. That is the finding, not a
gap in the stand-in. To show the stand-in's own load term, with the limit gate
raised so it is not the variable:

```
python scripts\capture_standin_slowdown.py
```

189 calls, three rounds at 1, 2, 4, 8, 16 and 32 concurrent, same prompt size
as a real call. Nearest-rank p50 single-call total (the convention Table 2a
uses): **696 ms** at 1 in flight, **2,255 ms** at 32 (p95 3,049 ms);
`results/standin_slowdown.csv` has every row. The term is
`1 + max(0, concurrency - 2) * 0.12` in `cmd/standin`, so the curve follows the
config, and raising the limits is what isolates it from the 429 path.

## The post-hoc repair run (`+fix`), and what it can and cannot claim

After the chosen 20-question sweep finished, four answers had been withheld on
questions the papers answer. Each was traced to a different mechanism, and each
mechanism has a switch that can be turned on without touching retrieval:

| Q | What was asked | Why it was withheld | Switch |
|---|---|---|---|
| q07 | Moshi's two audio streams | checker demanded a citation for every sentence, not every factual claim | `LenientCheck` |
| q08 | Inner Monologue | same checker path; the fact and page were in the draft | `LenientCheck` |
| q20 | which paper calls tokenization a barrier | planner guessed `attention`, so the citation guard rejected a correct `[chunking p.1]` | `ScopeFromQuestion` |
| q13 | cross-paper latency comparison | draft budget (700) consumed by 698 reasoning tokens, no content, three times | `-draftmaxtokens 1200` |
| q15 | Moshi text-tokenizer size | paraphrase gap: "vocabulary size" versus "contains 32,000 elements" | **none — left as a limit** |

q15 is not fixable by a setting, and the measurement says so: for the planner's
query the fact-carrying chunk ranks 11th by BM25 and 13th after reranking in the
mixed pool, and 17th even when retrieval ranks inside Moshi alone (169 chunks).
Ranking cannot bridge "vocabulary size" to "32,000 elements" without a synonym
step or a model call, and both would change the retrieval setting under test.

`-variant fixed` turns on the first two switches; `-draftmaxtokens` is separate
so the budget change is visible in the command line. Both default off, so every
number already published reproduces from the shipped binary unchanged, and each
fixed row records `"variant": "fixed"` in its detail line.

One run, all twenty questions:

`p220_k6_ryes` with `-variant fixed -draftmaxtokens 1200`, on `questions.json`
— the same twenty questions, the same retrieval setting, the same models as the
baseline. Only the three switches differ, so the pair is directly comparable and
the before/after covers every failure, including q13, q15 and q20, which the
ten-question subset does not contain.

An earlier plan split this into a ten-question row for Table 3 plus a
three-question probe. It was dropped: a ten-question row would have been
comparable to the p120/p450 rows but blind to three of the five diagnosed
failures, and a three-question probe is not an accuracy estimate at all. One
full twenty-question run costs about twice the tokens and answers the actual
question — "were the refusals a setting or a limit?" — for the whole set.

Where the row appears: `results/quality_fix20.detail.jsonl`, tagged
`"variant": "fixed"` on every line. It is reported next to the baseline and in
prose, never merged into the baseline totals.

**The honest limit of this evidence:** the switches were chosen after reading
the failures, so the `+fix` row is in-sample. It answers "was the withholding a
setting or a system limit?", not "what accuracy would this design achieve on new
questions?". A reviewer should read the row that way, and the report says so.
