# The Relay — write-up

Pipeline `cmd/relay` (Go): understand → retrieve → decide → draft → check;
stand-in `cmd/standin`; papers 1706.03762v7, 2410.00037v1, 2507.07955v2; raw
rows `results/results.csv`; key git-ignored.

## 1. What I built and why

Five steps: **understand** (LLM → query + paper) · **retrieve** (**tool call**:
BM25 + rerank + paper filter) · **decide** (**branch**: refuse on low coverage
before drafting) · **draft** (streamed, claims as `[paper p.N]`) ·
**check** (**loop**: verify, one revision).

**The decision that shaped it:** the trace showed **6,386 tokens/request**
(6,148 in the draft prompt) against **8,000 tokens/minute** — **~1.25
requests/minute**, so tokens, not requests, bound. I **routed each step to the
cheapest capable model** (62 vs 370 tokens for the same plan) and **fixed the
chunker** (sentence-boundary chunks, not 524-word ones): the draft prompt fell
**6,148 → 2,049**. A delivered answer still costs **TBD_REAL_TOK tokens** end to
end — no cheaper than before: three calls, refusals and revisions eat the 2.2×
prompt cut.

## 2. The real service and my stand-in

**Table 1: Limits you found in the real service**

| Limit | Value you found | How you found it | Confidence (high, medium, low) |
|---|---|---|---|
| Requests per minute | 30 | 429 body: `Limit 30, Used 30, Requested 1` (`results/real_burst.jsonl`) | high |
| Tokens per minute | 8,000 | `rl.limit_tokens` (response header) in `results/real_limits_probe.jsonl` | high |
| Requests per day | 1,000 | `rl.limit_requests` (response header), same file | high |
| Tokens per day | 200,000 | 429 body: `Limit 200000, Used 199927, Requested 153` (`results/real_probe.jsonl`) | high |
| Requests in flight at once | ~25 | 40 fired at once → 25 ok, 15 rate-limited (`real_burst.jsonl`) | medium |
| What it returns when a limit is hit | HTTP 429 + `retry-after`; JSON `type` `"requests"` or `"tokens"` | captured 429 bodies | high |
| Time until it recovers | RPM `retry-after: 2 s`; the daily-cap 429 asked 34.56 s; the day bucket refills at 2.31 tokens/s | `real_burst.jsonl`, `real_probe.jsonl` | high |

**The stand-in** (`cmd/standin`, fitted by `scripts/calibrate.py` to those
probes) is an OpenAI-compatible SSE server: slow, failing, refusing on a limit,
slow under load — p50 **696 ms** at 1 in flight, **2,255 ms** at 32 — streaming
token by token. **Least like it:** it cannot grade quality, has no reasoning
tokens, and resets its windows on a strict 60 s where the real ones roll.

## 3. Results

Load ran on the **stand-in**, quality on the **real service**. Percentiles are
nearest-rank (item ceil(p·n/100)); Table 2a uses `ok` rows, Table 3 graded answers,
because a refusal's wait is still a wait. Peak queue is the largest overlap of
`[start_time, start_time+total_ms)`; every cell recomputes from `results.csv`.

**Table 2a: Latency, one row per load scenario**

| Scenario | Real or stand-in | Requests sent | Duration (s) | Offered rate (per min) | First token p50 / p95 / p99 (ms) | Total time p50 / p95 / p99 (ms) |
|---|---|---|---|---|---|---|
| steady | stand-in | 62 | 182.0 | 20 | 3908.8 / 7088.9 / 7088.9 | 4355.5 / 7565.1 / 7565.1 |
| burst | stand-in | 68 | 127.1 | 6, then +60 in one burst | 997.4 / 997.4 / 997.4 | 1427.8 / 1427.8 / 1427.8 |
| sustained overload | stand-in | 178 | 60.4 | 180 | 978.0 / 8438.9 / 8438.9 | 1398.9 / 8864.9 / 8864.9 |
| per-minute limit halved | stand-in | 119 | 120.6 | 60 | 1009.5 / 4016.0 / 4016.0 | 1451.2 / 4574.2 / 4574.2 |
| daily budget exhausted | stand-in | 30 | 45.7 | 40 | 962.6 / 962.6 / 962.6 | 1445.5 / 1445.5 / 1445.5 |

**Table 2b: Outcomes, same rows**

| Scenario | Succeeded % | Rate-limited % | Timed out or given up % | Retries per request | Peak queue length | Tokens per answer | Cost per 1,000 answers |
|---|---|---|---|---|---|---|---|
| steady | 12.9 | 0.0 | 87.1 | 0.00 | 3 | 3664 | $0.4098 |
| burst | 2.9 | 0.0 | 97.1 | 0.00 | 60 | 3445 | $0.3934 |
| sustained overload | 2.2 | 0.0 | 97.8 | 0.00 | 3 | 3584 | $0.4038 |
| per-minute limit halved | 3.4 | 0.0 | 96.6 | 0.00 | 3 | 3515 | $0.3986 |
| daily budget exhausted | 3.3 | 96.7 | 0.0 | 0.00 | 2 | 3484 | $0.3963 |

**The number I could not have guessed:** one answer costs **6,364 tokens** on
the real service — **80% of a minute's budget** — so 8,000 tokens/minute buys
**1.3 answers, not 30**, and at a steady **20/minute** only **5.9%** were
answered; the rest were refused locally in **16.3 ms** for **zero tokens** (p50
across all 457 give-ups). Admission
reserves 6,286 tokens — **47 s** of budget — so above ~1.3/minute only the shed
rate changes: four rows time a single answer (p50 = p95 = p99). `Tokens per
answer` stays flat (5,382–6,046) and 1,000 answers cost ≈**$0.55**: **0 errors,
0 mid-flight timeouts** in 503 requests, and **486 of 503** were given up before
any provider call: **457** by admission control and **29** by the daily budget.

**Reading note.** Cost is notional (**$0.075/M in, $0.30/M out**; billed
**$0.00**). Only `source=real` rows are graded; tokens/answer divides all spend
by graded answers. Table 2b's tokens are the stand-in's character count (5,626,
within 12% of the real 6,364); duration and rate are inputs
(`results/scenario_meta.json`).

## 4. Where the time goes

**Inside a call:** five probes took **493–740 ms**; the one with a server
breakdown spent **513 of 951 ms in Groq's `queue_time`**. **Inside a
request** on the real service: three calls plus local retrieval — median
TBD_P50 ms, first token at TBD_TTFT ms. **In front of the request:** admission.
One slow request, `TBD_REQ` (stand-in, `results/load_detail.jsonl`), took
**TBD_REQ_TOTAL ms**: **TBD_REQ_WAIT ms waiting** (TBD_REQ_WAITPCT%) for a
worker slot, **TBD_REQ_RETRY ms retrying** (TBD_REQ_RETRYPCT%), **TBD_REQ_WORK
ms working** (TBD_REQ_WORKPCT%). The three
fields sum to `total_ms`; that is how I know the split. First token at
TBD_REQ_TTFT ms — **TBD_REQ_TTFTPCT% of its time before a token** — so 991 ms of
the working time was plan, retrieval and prefill, and 499 ms decode and check.
**Outside it:** the daily bucket refills at 2.3 tokens/s; the harness recorded
**17–90 minutes of pacing and waiting per question** (`pacing_ms` + `waiting_ms`).

## 5. When a limit is hit

**Delayed:** if a minute with room for the **whole request's token cost**
(6,286: one plan+draft+check pass from q01's **6,123**; revisions exceed it) is **within 8 s**, the caller waits.
**Dropped:** beyond that, the caller **refuses now** (`status=timeout`: a give-up
before the call, not a mid-flight timeout) — **457** of 503 requests, returning
local pointers in **16.3 ms p50 / 89.9 ms p95** for **0 tokens**; **29** more
were dropped by the daily budget, **486 of 503 in all**. **Per-day** is a hard
stop: refilling at 2.3 tokens/s means hours; the pipeline sends nothing and
returns §7's reply. **Who gets a weaker answer:** only the caller after the daily
budget — pointers, not an answer (§7); at the minute limit, nobody. The one
degradation is the planner fallback, labelled *unrouted*.

## Retrieval: quality versus cost

Quality needs real calls: each extra setting costs about a day of the
200,000-token budget, so the chosen setting ran 20 questions and the two
comparisons the same ten. Stand-in rows are **not measured** (canned).
**Gold-sentence recall** asks whether the retrieved context contains the answer
sentence (17 answerable questions, no model call).

**Table 3: Retrieval settings (at least three)**

| Setting (piece size, pieces fetched, re-rank yes or no) | Questions asked | Correct and cited % | Wrong % | Correctly refused % | Tokens per answer | First token p50 / p95 (ms) | Total time p50 / p95 (ms) |
|---|---|---|---|---|---|---|---|
| p220_k6_ryes **(real)** | 20 | 45.0 | 15.0 | 20.0 | 2939 | 21449 / 36800 | 37301 / 72963 |
| p120_k4_rno (stand-in) | 20 | not measured | not measured | not measured | 2039 | 915 / 956 | 1480 / 1549 |
| p220_k6_ryes (stand-in) | 20 | not measured | not measured | not measured | 3569 | 1001 / 1121 | 1559 / 1675 |
| p220_k6_rno (stand-in) | 20 | not measured | not measured | not measured | 3539 | 995 / 1224 | 1568 / 1779 |
| p450_k3_ryes (stand-in) | 20 | not measured | not measured | not measured | 3042 | 988 / 1474 | 1550 / 2003 |
| p900_k2_ryes (stand-in) | 20 | not measured | not measured | not measured | 3106 | 1017 / 1274 | 1560 / 1806 |

The stand-in's **85.0%** (17/20) on the automatic citation grade is not quality:
its draft is a fixed sentence plus a supplied citation. Recall discriminates:
**12/17** at k=6, 11 at k=4, 10 at k=3, 9 at k=2; re-rank changes nothing. The
two extra pieces buy **q19**, the adversarial threshold question.

**Chosen: `p220_k6_ryes`** — best recall (12/17), measured on all 20 questions:
**6,364 tokens/answer** (TBD_CORRECT% correct and cited, TBD_WRONG% wrong; the
three columns omit `refused_wrongly`, **TBD_REFW%**). On the ten-question subset
p450 answers **9/10**, p220 **7/10**, p120 **6/10**; I kept p220 because p450
costs **+590 ms** to first token and recalls **10/17 vs 12/17** across all
seventeen answerable questions (no model, less overfit). The cheapest, `p120_k4_rno` (TBD_P120_TOK tokens; 573
context words vs 1,279), measures TBD_P120_CORRECT% on the same ten and raises
the ceiling from 2.6 to TBD_CHEAP_ANS answers/minute, at 11/17 recall.
Re-rank (+1.3% tokens) stays for page quality. **When a limit is close:** I keep
`p220_k6_ryes` rather than shrinking to `p120` — if 6,286 tokens will not clear
in 8 s (or the day cap is gone), local BM25 runs unrouted across all three
papers for 0 tokens and returns §7's pointers.

**Repair run (in-sample, reported separately).** Same twenty questions and
setting with two post-hoc flags (`-variant fixed -draftmaxtokens 1200`) chosen
after reading these failures — in-sample, not an estimate. Correct on the
seventeen answerable questions went **TBD_BASE_CORRECT
(TBD_BASE_ANS_PCT%) → TBD_FIX_CORRECT (TBD_FIX_ANS_PCT%)**; over-refusals
**TBD_BASE_RW → TBD_FIX_RW**; wrong answers **TBD_BASE_WRONG → TBD_FIX_WRONG**.
q08, q20 and previously unmeasured q13 now answer; q11 regressed to a refusal;
q15 held; q07 turned from a refusal into a confident wrong answer — quoting the
streaming-TTS model's 2-second audio delay (p.21) for the dialogue model's
two-stream design (p.15). Loosening the checker bought two answers and one
fabrication.

## How this library will mislead a naive retriever

1. **Formatting hides the token.** *"What is d_model in the base Transformer?"*
   — p.3 prints `dmodel`; the query says `d_model`, BM25 never matches, and the
   gold sentence is absent (a measured recall miss on every setting).
2. **The question's word is not the paper's.** *"How many elements does Moshi's
   text-tokenizer vocabulary contain?"* — p.7 says the SentencePiece unigram
   tokenizer contains **32,000 elements**; "vocabulary" and "how many" do not
   appear nearby, and the chunk ranks 17th in Moshi. The pipeline refused an
   answerable question.
3. **Right fact, wrong paper and sub-model.** *"What optimizer and learning rate
   did the authors use?"* — Attention uses **Adam** (β1=0.9, β2=0.98, p.7);
   Moshi and Dynamic Chunking use **AdamW** (p.11 each), and Moshi alone has
   three rates: codec **8e-4** (p.11), Helium **3e-4** (p.20), fine-tuning
   **2e-6** (p.21).

## 6. Retries

Policy: retry `rate_limited` and transient errors **3 times (4 attempts)**,
backoff 0.5→1→2 s capped at 12 s, honouring `retry-after`; a deterministic 4xx
is never retried. **Measured: 0 retries in all six load shapes (503 requests)**
— admission reserves the window, so the provider is never handed a call it would
reject. The path is real: 15 of 40 simultaneous probes got `retry-after`; forced
to 500s, the pipeline spent **6 retries and 7.1 s, then refused**
(`results/standin_faults.jsonl`). **Retrying made it worse:** the first sweep
retried the daily-budget 429 six times — waits of 300–742 s, **47 minutes for
zero answers** — because each retry re-spent the planner and draft
(`results/quality_real_run.attempt1.log`).

## 7. What the person sees

At 3pm the daily budget is gone; the reply is immediate — **11.9 ms** median
(n=5, 7.9–18.4), no spinner, no model call
(`results/daily_exhausted_reply.jsonl`):

> **Daily model quota reached.** I cannot verify a generated answer until the
> provider's daily quota resets, so I will not guess. The planning step could
> not run, so these are the best lexical match from each paper, not a
> paper-routed answer: Dynamic Chunking… **[chunking p.37]** · Moshi…
> **[moshi p.62]** · Attention… **[attention p.9]**.

No delay, no wrong answer — one real pointer per paper, labelled unrouted.

## 8. What I'm unsure about, what I left out, what I learned

**Least certain assumption:** that a lenient checker is safe. It bought q08, q20
and q13, and it also let q07 through as a confident wrong answer. If a wrong
answer costs more than a refusal — the brief says it does — the strict checker
is the right default, and the lenient one should ship only behind a second check
that the cited page contains the claim. **Not built:** vector embeddings (BM25
is local and free) and fine-tuning (out of scope). **New to me:** a free-tier
*token* budget makes the unit tokens per request, not requests per minute; one
request is most of a minute.

## Your feedback on Vaani

Full version in `results/vaani_feedback.md`. **Stood out:** I could *hear* it
before reading — right for audio. **Confused me:** three pages to learn its
*shape*; I want what I install above the fold, and time-to-first-audio-byte and
talk-over rate are implied, not stated. **One change:** put the demo transcript
*with timings* beside the audio, so turn boundaries are visible. *Caveat:* site
read only.

## How I worked with the assistant

Full transcript in `results/conversation_history.md` (key redacted). **Three
prompts that mattered:** *measure the real limits* — it reframed 30
requests/minute as 8,000 tokens/minute, 1.25 requests' worth; *print chunk
statistics* — 524-word chunks became sentence-boundary chunks, 3× fewer draft
tokens; *grade every answer against the page it cites* — the automatic grade
called 15/20 correct in the repair run, hand grading found 13, because q07 and
q09 cite pages that do not carry the claim. **One thing it got wrong:** it said
the remaining refusals were the old 200-character truncation. That bug was real
but already fixed (`maxCheckPassageChars` 4,000, pinned by
`TestCheckPromptCarriesFullPassage`; q02's fact sits at char ~900). The actual
cause was the checker demanding a citation on every sentence; `-variant fixed`
showed it — q08 and q20 answered, q07 came back wrong. **One decision I overruled:** an independent AI audit wanted a
fourth Table 3 column (to close the `refused_wrongly` gap); I kept the brief's
schema and reported the **TBD_REFW%** share in prose.
**Distinctly mine:** the 20 questions and ground truth, four honesty calls (a
stand-in cannot grade quality; a right fact on a worse page counts; a budget
refusal is not a guess; cost is reported), and the refusal-over-degradation rule.
