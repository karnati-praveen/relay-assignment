## Table 1: Limits you found in the real service

| Limit | Value you found | How you found it | Confidence (high, medium, low) |
|---|---|---|---|
| Requests per minute | 30 | 429 body: `Limit 30, Used 30, Requested 1` (`results/real_burst.jsonl`) | high |
| Tokens per minute | 8,000 | `rl.limit_tokens` (response header) in `results/real_limits_probe.jsonl` | high |
| Requests per day | 1,000 | `rl.limit_requests` (response header), same file | high |
| Tokens per day | 200,000 | 429 body: `Limit 200000, Used 199927, Requested 153` (`results/real_probe.jsonl`) | high |
| Requests in flight at once | ~25 | 40 fired at once → 25 ok, 15 rate-limited (`results/real_burst.jsonl`) | medium |
| What it returns when a limit is hit | HTTP 429 + `retry-after`; JSON `type` `"requests"` or `"tokens"` | captured 429 bodies | high |
| Time until it recovers | RPM `retry-after: 2 s`; the daily-cap 429 asked 34.56 s; the day bucket refills at 2.31 tokens/s | `results/real_burst.jsonl`, `results/real_probe.jsonl` | high |

## Table 2a: Latency, one row per load scenario

| Scenario | Real or stand-in | Requests sent | Duration (s) | Offered rate (per min) | First token p50 / p95 / p99 (ms) | Total time p50 / p95 / p99 (ms) |
|---|---|---|---|---|---|---|
| sustainable (capacity) | stand-in | 7 (7 ok) | 421.9 | 1 | 1003.8 / 7208.7 / 7208.7 | 1634.0 / 7802.3 / 7802.3 |
| steady | stand-in | 101 (6 ok) | 302.9 | 20 | 6516.4 / 8562.7 / 8562.7 | 7122.5 / 9062.1 / 9062.1 |
| burst | stand-in | 68 (1 ok) | 127.1 | 6, then +60 in one burst | 1014.8 / 1014.8 / 1014.8 | 1537.5 / 1537.5 / 1537.5 |
| sustained overload | stand-in | 178 (1 ok) | 60.3 | 180 | 917.8 / 917.8 / 917.8 | 1473.2 / 1473.2 / 1473.2 |
| per-minute limit halved | stand-in | 119 (1 ok) | 120.6 | 60 | 962.6 / 962.6 / 962.6 | 1551.4 / 1551.4 / 1551.4 |
| daily budget exhausted | stand-in | 30 (1 ok) | 45.7 | 40 | 947.8 / 947.8 / 947.8 | 1526.6 / 1526.6 / 1526.6 |

Latency cells cover only requests that produced an answer (`status=ok`); the `n ok` beside `Requests sent` is that cell's sample size. Percentiles are nearest-rank: item ceil(p*n/100), counting from 1, of the sorted answered requests, so every cell is an observed request and recomputes from results.csv. With n ok < 20 p95 and p99 are the same request; with n ok = 1 all three are. The shed/refused requests are counted in Table 2b.

## Table 2b: Outcomes, same rows

| Scenario | Succeeded % | Rate-limited % | Timed out or given up % | Retries per request | Peak queue length | Tokens per answer | Cost per 1,000 answers |
|---|---|---|---|---|---|---|---|
| sustainable (capacity) | 100.0 | 0.0 | 0.0 | 0.00 | 1 | 5646 | $0.5585 |
| steady | 5.9 | 0.0 | 94.1 | 0.00 | 2 | 5623 | $0.5567 |
| burst | 1.5 | 0.0 | 98.5 | 0.00 | 60 | 6046 | $0.5885 |
| sustained overload | 0.6 | 0.0 | 99.4 | 0.00 | 2 | 5382 | $0.5386 |
| per-minute limit halved | 0.8 | 0.0 | 99.2 | 0.00 | 2 | 5382 | $0.5386 |
| daily budget exhausted | 3.3 | 96.7 | 0.0 | 0.00 | 2 | 5382 | $0.5386 |

`Rate-limited` is the provider's own 429. `Timed out or given up` includes the `timeout` rows that our admission control refused before spending a provider call, in milliseconds, rather than queueing them past the scenario. `Tokens per answer` and `Cost per 1,000 answers` count every token the scenario spent, divided by the answers it actually delivered.

## Table 3: Retrieval settings (at least three)

Quality columns need the real service; the stand-in returns canned text, so its quality cells are "not measured". Tokens and latency are comparable across both. The two comparison settings use the first ten questions (q01–q10); the chosen setting uses all twenty.

| Setting (piece size, pieces fetched, re-rank yes or no) | Questions asked | Correct and cited % | Wrong % | Correctly refused % | Tokens per answer | First token p50 / p95 (ms) | Total time p50 / p95 (ms) |
|---|---|---|---|---|---|---|---|
| p220_k6_ryes **(real)** | 20 (19 graded) | 57.9 | 5.3 | 15.8 | 6364 | 1698.3 / 9448.5 | 2583.4 / 34689.1 |
| p120_k4_rno **(real)** | 10 | 60.0 | 10.0 | 0.0 | 3682 | 1411.6 / 9335.9 | 2616.4 / 26404.7 |
| p450_k3_ryes **(real)** | 10 | 90.0 | 10.0 | 0.0 | 6081 | 2288.3 / 3903.3 | 3935.8 / 27414.2 |
| p220_k6_rno (stand-in) | 20 | not measured | not measured | not measured | 5555 | 954.6 / 1176.2 | 1507.4 / 1721.3 |
| p900_k2_ryes (stand-in) | 20 | not measured | not measured | not measured | 5026 | 959.5 / 1062.3 | 1497.1 / 1591.1 |

## Table 3b: Retrieval recall (offline, no model call)

| Setting | Answerable questions | Gold sentence in context % | Context words (mean) | Pieces (mean) |
|---|---|---|---|---|
| p120_k4_rno | 17 | 64.7 | 573 | 4.00 |
| p220_k6_ryes | 17 | 70.6 | 1279 | 5.59 |
| p220_k6_rno | 17 | 70.6 | 1288 | 5.59 |
| p450_k3_ryes | 17 | 58.8 | 1201 | 3.00 |
| p900_k2_ryes | 17 | 52.9 | 1287 | 2.00 |

`Gold sentence in context` is a property of retrieval alone: it does not say the model used the sentence, only that the setting retrieved it. The patterns are hand-written from the pinned text (`gold` in `data/questions.json`).

## Where the time goes (real service, chosen setting)

| Metric | p50 | p95 | max |
|---|---|---|---|
| Total (ms) | 2583.4 | 34689.1 | 34689.1 |
| First token (ms) | 1698.3 | 9448.5 | 9448.5 |
| Tokens/answer (graded row) | 5476.0 | 12440.0 | 12440.0 |

- Median first-token share of total: **66%** (1698 of 2583 ms) — i.e. most of a slow answer is spent *before* any token arrives, which is waiting, not model work.
- Questions measured: 19 (of 20 asked)
- Tokens per answer (all sweep spend / 19 graded): **6364** (in 5294, out 1069)
- Median pacing wait before a question (outside its total): **2349 s**

## Answer quality (real service)

Automatic citation grade (`grade_auto` in `quality_real.detail.jsonl` — checks only that a citation parsed; `results.csv` carries the hand-verified grade below):

| Grade | Count | % of evaluated |
|---|---|---|
| correct | 12 | 60.0 |
| wrong | 0 | 0.0 |
| refused_correctly | 3 | 15.0 |
| refused_wrongly | 5 | 25.0 |
| not measured (daily budget stopped) | 0 | — |

- Evaluated by `grade_auto`: **20 of 20** (`q07, q08, q13, q20` had `status=error` after checker JSON retry exhaustion, counted as `refused_wrongly` by `grade_auto` and reclassified `not measured` in hand grading).

The automatic grade is deliberately coarse: it cannot tell a right fact from a wrong one, and it treats any cited answer as correct. The hand-verified fact grade is reconciled separately in `results/quality_manual.md`; `results/results.csv`, Table 3, and the report use that hand-verified grade over the 19 evaluated questions.

### Hand-verified fact grade (Table 3 and `results.csv` use this)

| Verdict | Count | % of 19 graded |
|---|---|---|
| correct | 11 | 57.9 |
| wrong | 1 | 5.3 |
| refused_correctly | 3 | 15.8 |
| refused_wrongly | 4 | 21.1 |
| not measured (unanswered operational error — q13: status=error) | 1 | — |

- Automatic and hand grades disagree on **1 of 19** rows: q09 auto=correct manual=wrong. A cited answer is not automatically a right answer; the table above is the one the report quotes.
- Citation page is the best page for the fact: **10 of 19**.

