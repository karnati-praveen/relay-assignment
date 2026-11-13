# The stand-in model service

`cmd/standin` is a local HTTP server that speaks the same OpenAI-compatible,
streaming-SSE chat protocol as Groq, so the pipeline talks to it with no code
changes. It exists to run the load shapes the real free tier cannot take.

Run it:

```
go run ./cmd/standin -addr 127.0.0.1:8099 -config standin.json
```

## What it can do (all required by the brief)

- **be slow**: per-token decode delay plus a first-token delay.
- **fail**: `error_prob` returns HTTP 500.
- **refuse because a limit was reached**: RPM, TPM, RPD and TPD buckets, each
  returning a 429 with `retry-after` and the same JSON error shape (including a
  distinct `type` of `"requests"` or `"tokens"`) as the real service.
- **become slower under load**: per-request latency is multiplied by
  `1 + (concurrency - concurrency_ref) * slowdown_per_conc`.
- **send its reply a piece at a time**: real SSE chunks, one token piece each,
  with the usage object in a final chunk, exactly like Groq.

## How it was calibrated

`scripts/calibrate.py` fits `standin.json` from the real measurements in
`results/real_probe.jsonl` and `results/real_burst.jsonl`:

| Parameter | Value | Derived from |
|---|---|---|
| `queue_mean_ms` | 314 | median server-reported `queue_time` of real calls |
| `ttft_base_ms` | 79 | median real TTFT minus the queue component |
| `per_tok_ms` | 1.11 | median real ms per completion token |
| `latency_sigma_ms` | 0.084 | spread of log real total latency |
| `slow_tail_prob` / `_extra_ms` | 0.05 / 110 | real max minus real median |
| `slowdown_per_conc` | 0.12 | from the burst: ~25 of 40 in flight tolerated |
| `rpm`/`tpm`/`rpd`/`tpd` | 30 / 8000 / 1000 / 200000 | real limits |

## Where it is least like the real thing

1. **It cannot produce real answers.** The reply is a fixed, plausible,
   cited-looking sentence whose only job is to exercise the pipeline's parsing,
   citation and check paths. So it is useless for measuring answer quality, and
   a purely automatic grader will mark every answerable question "correct"
   against it. **All quality numbers in this submission come from the real
   service.**

2. **Its latency model is a formula, not a queue.** Load-dependent slowdown is
   a linear multiplier on concurrency. The real service's latency under load is
   whatever its scheduler does; I only ever measured it up to the point where it
   started returning 429s, so beyond that the stand-in's shape is a guess.

3. **It has no real tokenizer.** Tokens are estimated from character count, so
   its TPM accounting is approximate, whereas the real service counts real
   tokens and its TPM is exact.

4. **It never has an unexplained slow success.** The real service occasionally
   succeeds slowly (my probe saw totals 417-562 ms with no cause I could see);
   the stand-in's slowness is always attributable to a parameter.

5. **429 timing differs.** The real service's RPM window recovered in ~2 s and
   its TPM window in up to ~9 s, with `retry-after` as low as 2. The stand-in
   resets a strict 60-second window aligned to the wall-clock minute (the
   client's admission mirror uses the same alignment), so it is *stricter* and
   more regular than the real thing.
