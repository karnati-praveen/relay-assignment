# Real-service measurements (Groq free tier)

All taken on 2026-11-10 with my own free account, model `openai/gpt-oss-20b`
(and `openai/gpt-oss-120b`, `qwen/qwen3.8-27b` for comparison).
Raw files: `real_probe.jsonl` (daily-limit capture), `real_limits_probe.jsonl` (successful call with the
per-minute/day headers), `real_burst.jsonl` (40 at once), `structured_probe_*.jsonl` (shaped calls).

## Limits (from response headers and 429 bodies)

| Limit | Value | Evidence |
|---|---|---|
| Requests per minute (RPM) | 30 | 429 body: `Limit 30, Used 30, Requested 1` (`real_burst.jsonl`) |
| Tokens per minute (TPM) | 8,000 | header `x-ratelimit-limit-tokens: 8000` (`real_limits_probe.jsonl`) |
| Requests per day (RPD) | 1,000 | header `x-ratelimit-limit-requests: 1000` (same file) |
| Tokens per day (TPD) | 200,000 | 429 body: `Limit 200000, Used 199927, Requested 153` (`real_probe.jsonl`) |
| Requests in flight | not documented; observed 40 concurrent accepted 25 | burst test |

All three text models share the same base limits, but **each model has its own
token bucket**: after exhausting gpt-oss-20b TPM, gpt-oss-120b still reported
`x-ratelimit-remaining-tokens: 7917`.

## What a limit returns

- HTTP `429`, with `retry-after` header (2 s for RPM, up to ~9 s for TPM).
- JSON error body with a distinct `type` field: `"requests"` for RPM, `"tokens"`
  for TPM, both `code: "rate_limit_exceeded"`.
- Rate-limited calls fail **fast**: ~215 ms (RPM) / ~100-166 ms (TPM), not slowly.

## Recovery

- RPM window is 60 s; `retry-after: 2` and a retry within the same minute succeeded.
- TPM window is rolling; `retry-after` was up to 9 s and
  `x-ratelimit-reset-tokens` ~51 s. The bucket does not refill instantly.

## Latency of a small call (five shaped probes)

`structured_probe_long300.jsonl` and `structured_probe_stream300.jsonl`, each a
300-token-max call shaped like one pipeline step:

- Total: **493–740 ms**.
- Server-reported `queue_time` is **258–448 ms** of that, i.e. **a large share of
  wall time is queueing inside Groq, not compute**; prompt processing is 12–32 ms.
- Completion: 88–166 tokens.

`real_probe.jsonl` holds the deliberate daily-limit capture: one request refused
with the TPD 429 quoted above. `real_limits_probe.jsonl` holds a successful call
with the per-minute and per-day headers attached.

## The binding constraint

A realistic pipeline draft call before the chunker fix (6 passages ~220 words +
system prompt) cost **2,029 prompt + 448 completion = 2,477 tokens**, and a full
pipeline request (understand + draft + check) cost **5,672 in + 545 out = 6,217
tokens**. Against an 8,000 TPM budget that is **~1.3 pipeline requests per
minute**. TPM, not RPM, is what limits this workload.

The trace of the same pre-fix design estimated **6,386** tokens per request
(character-counted on the stand-in, hence the 3% difference from the real
service's own 6,217); the write-up quotes the trace figure because it is the one
that breaks down per step.

After sentence-boundary chunking cut the draft prompt, the measured real sweep
averaged **6,364 tokens per answer**, i.e. **1.3 answers per minute** and at
most ~31 answers per 200K-token day. The daily budget is not a midnight reset:
it is a refilling bucket. `Used 199927, Requested 153` produced
`try again in 34.56s`, which is `(199927+153-200000)/34.56s ≈ 2.31 tokens/s` —
exactly 200,000 tokens spread over 24 hours.

## The sweep that never converged

Waiting out a daily limit by honouring only the provider's `retry-after` does not
work, and the reason is arithmetic, not luck. `retry-after` funds *the call that
was refused*; the retry then restarts the request from the planner, so it spends
the planner and draft tokens a second time. Each cycle therefore ends with less
headroom than it began with. Measured: six waits totalling 47 minutes
(300+742+300+431+300+738 s, `quality_real_run.attempt1.log`), ~2,300 tokens
burned per cycle, and no answer.

The fix waits for a whole attempt: `retry_after + spent_in_this_attempt/2.31`
seconds (`cmd/relay/budget_test.go`), and paces between questions by the same
rule so the limit is usually never hit (`-tpd`). Both are measurement-harness
behaviour; the serving path still refuses immediately (§5 of the write-up).

## The first full attempt: a question can cost nothing, or double

Two rows from the next attempt (`quality_real_run.attempt2.log`) set the
harness's remaining rules.

**q01 cost nothing.** `dial tcp: lookup api.groq.com: no such host`, then two
90 s timeouts: 0 tokens, 120 s, no answer. That is a network event, not a
measurement of the system, so it must not take the question's only slot. The
harness now retries a zero-token transport failure up to three times before
recording anything.

**q02 cost double.** The checker rejected a correct answer as unsupported
(`could not verify the answer: unsupported: The claim that ...`), so the
pipeline spent its one allowed revision: two draft calls and two check calls,
**6,120 in + 1,381 out = 7,501 tokens** against the 3,888-token reservation in
force at the time (the reservation is now 6,286, sized from the measured
6,123). At 2.31 tokens/s that is 54 minutes to earn back, not 28. Pacing
therefore seeds itself from the previous question's *actual* spend (on resume
too), not from the reservation — a revision is a real cost, and pretending
otherwise is how the first attempt diverged.

That row is also the archived evidence of the checker bug: the revision was
caused by the checker reading only 200 characters of each passage, not by a
wrong answer (§"The checker bug that invalidated the first sweep" in
`measurement_plan.md`, rows in `archive_checkbug/`).

The two rows also explain the spread in the write-up's cost per answer: the
floor is one draft plus one check (~6,100 tokens measured after the fix), the
ceiling is a revision (~12,000).
