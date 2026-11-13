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
**6,148 → 2,049**. A delivered answer still costs **6,364 tokens** end to
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
| sustainable (capacity) | stand-in | 7 (7 ok) | 421.9 | 1 | 1003.8 / 7208.7 / 7208.7 | 1634.0 / 7802.3 / 7802.3 |
| steady | stand-in | 101 (6 ok) | 302.9 | 20 | 6516.4 / 8562.7 / 8562.7 | 7122.5 / 9062.1 / 9062.1 |
| burst | stand-in | 68 (1 ok) | 127.1 | 6, then +60 in one burst | 1014.8 / 1014.8 / 1014.8 | 1537.5 / 1537.5 / 1537.5 |
| sustained overload | stand-in | 178 (1 ok) | 60.3 | 180 | 917.8 / 917.8 / 917.8 | 1473.2 / 1473.2 / 1473.2 |
| per-minute limit halved | stand-in | 119 (1 ok) | 120.6 | 60 | 962.6 / 962.6 / 962.6 | 1551.4 / 1551.4 / 1551.4 |
| daily budget exhausted | stand-in | 30 (1 ok) | 45.7 | 40 | 947.8 / 947.8 / 947.8 | 1526.6 / 1526.6 / 1526.6 |

**Table 2b: Outcomes, same rows**

| Scenario | Succeeded % | Rate-limited % | Timed out or given up % | Retries per request | Peak queue length | Tokens per answer | Cost per 1,000 answers |
|---|---|---|---|---|---|---|---|
| sustainable (capacity) | 100.0 | 0.0 | 0.0 | 0.00 | 1 | 5646 | $0.5585 |
| steady | 5.9 | 0.0 | 94.1 | 0.00 | 2 | 5623 | $0.5567 |
| burst | 1.5 | 0.0 | 98.5 | 0.00 | 60 | 6046 | $0.5885 |
| sustained overload | 0.6 | 0.0 | 99.4 | 0.00 | 2 | 5382 | $0.5386 |
| per-minute limit halved | 0.8 | 0.0 | 99.2 | 0.00 | 2 | 5382 | $0.5386 |
| daily budget exhausted | 3.3 | 96.7 | 0.0 | 0.00 | 2 | 5382 | $0.5386 |

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
2,583.4 ms, first token at 1,698.3 ms. **In front of the request:** admission.
One slow request, `steady-54` (stand-in, `results/load_detail.jsonl`), took
**9,062.1 ms**: **7,571.3 ms waiting** (83.5%) for a
worker slot, **0.0 ms retrying** (0.0%), **1,490.7
ms working** (16.5%). The three
fields sum to `total_ms`; that is how I know the split. First token at
8,562.7 ms — **94.5% of its time before a token** — so 991 ms of
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
| p220_k6_ryes **(real)** | 20 (19 graded) | 57.9 | 5.3 | 15.8 | 6364 | 1698.3 / 9448.5 | 2583.4 / 34689.1 |
| p120_k4_rno **(real)** | 10 | 60.0 | 10.0 | 0.0 | 3682 | 1411.6 / 9335.9 | 2616.4 / 26404.7 |
| p450_k3_ryes **(real)** | 10 | 90.0 | 10.0 | 0.0 | 6081 | 2288.3 / 3903.3 | 3935.8 / 27414.2 |
| p220_k6_rno (stand-in) | 20 | not measured | not measured | not measured | 5555 | 954.6 / 1176.2 | 1507.4 / 1721.3 |
| p900_k2_ryes (stand-in) | 20 | not measured | not measured | not measured | 5026 | 959.5 / 1062.3 | 1497.1 / 1591.1 |

The stand-in's **85.0%** (17/20) on the automatic citation grade is not quality:
its draft is a fixed sentence plus a supplied citation. Recall discriminates:
**12/17** at k=6, 11 at k=4, 10 at k=3, 9 at k=2; re-rank changes nothing. The
two extra pieces buy **q19**, the adversarial threshold question.

**Chosen: `p220_k6_ryes`** — best recall (12/17), measured on all 20 questions:
**6,364 tokens/answer** (57.9% correct and cited, 5.3% wrong; the
three columns omit `refused_wrongly`, **21.1%**). On the ten-question subset
p450 answers **9/10**, p220 **7/10**, p120 **6/10**; I kept p220 because p450
costs **+590 ms** to first token and recalls **10/17 vs 12/17** across all
seventeen answerable questions (no model, less overfit). The cheapest, `p120_k4_rno` (3,682 tokens; 573
context words vs 1,279), measures 60.0% on the same ten and raises
the ceiling from 1.3 to 2.2 answers/minute, at 11/17 recall.
Re-rank (+1.3% tokens) stays for page quality. **When a limit is close:** I keep
`p220_k6_ryes` rather than shrinking to `p120` — if 6,286 tokens will not clear
in 8 s (or the day cap is gone), local BM25 runs unrouted across all three
papers for 0 tokens and returns §7's pointers.

**Repair run (in-sample, reported separately).** Same twenty questions and
setting with two post-hoc flags (`-variant fixed -draftmaxtokens 1200`) chosen
after reading these failures — in-sample, not an estimate. Correct on the
seventeen answerable questions went **11
(64.7%) → 13 (76.5%)**; over-refusals
**4 → 2**; wrong answers **1 → 2**.
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
schema and reported the **21.1%** share in prose.
**Distinctly mine:** the 20 questions and ground truth, four honesty calls (a
stand-in cannot grade quality; a right fact on a worse page counts; a budget
refusal is not a guess; cost is reported), and the refusal-over-degradation rule.
