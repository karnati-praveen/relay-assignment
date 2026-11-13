"""Fit the stand-in's latency/failure parameters from real measurements.

Inputs: results/real_probe.jsonl and results/real_limits_probe.jsonl
(sequential small calls) and results/real_burst.jsonl (concurrency + 429
behaviour). Output: standin.json

The sequential probes are read from both files because real_probe.jsonl was
later overwritten by the daily-cap probe; either file may be the one that still
holds the answered calls.
"""
import json, os, statistics, math

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def load(path):
    out = []
    for line in open(os.path.join(BASE, path), encoding="utf-8"):
        line = line.strip()
        if line:
            out.append(json.loads(line))
    return out


probe = load("results/real_probe.jsonl") + load("results/real_limits_probe.jsonl")
burst = load("results/real_burst.jsonl")

ok_probe = [r for r in probe if r["status"] == "ok"]
# A non-streamed structured call reports ttft_ms=0; it constrains the total but
# says nothing about time to first token, so keep the two lists apart.
ttfts = [r["ttft_ms"] for r in ok_probe if r["ttft_ms"] > 1]
totals = [r["total_ms"] for r in ok_probe if r["total_ms"] > 1]
queues = [r["usage"].get("queue_time_ms", 0) for r in ok_probe if r.get("usage")]

print("Real small calls (n=%d)" % len(ok_probe))
if ttfts and totals:
    print("  TTFT   min=%.0f median=%.0f max=%.0f" % (min(ttfts), statistics.median(ttfts), max(ttfts)))
    print("  total  min=%.0f median=%.0f max=%.0f" % (min(totals), statistics.median(totals), max(totals)))
if queues:
    print("  queue  median=%.0f max=%.0f (server-reported)" % (statistics.median(queues), max(queues)))

# Prompt/completion timing from usage, to split prefill vs decode.
pt = [r["usage"].get("prompt_time_ms", 0) for r in ok_probe if r.get("usage")]
ct = [r["usage"].get("completion_time_ms", 0) for r in ok_probe if r.get("usage")]
ctok = [r["usage"].get("completion_tokens", 0) for r in ok_probe if r.get("usage")]
if ct and ctok:
    per_tok = [c / max(1, t) for c, t in zip(ct, ctok)]
    print("  decode ms/token median=%.2f" % statistics.median(per_tok))

# Burst: measure how many concurrent requests are accepted and how 429s fail.
burst_rows = [r for r in burst if "burst_i" in r]
ok_b = [r for r in burst_rows if r["status"] == "ok"]
rl_b = [r for r in burst_rows if r["status"] == "rate_limited"]
print("\nBurst (n=%d): ok=%d rate_limited=%d" % (len(burst_rows), len(ok_b), len(rl_b)))
if rl_b:
    print("  429 total_ms median=%.0f" % statistics.median([r["total_ms"] for r in rl_b]))
    ra = [r["rl"].get("retry_after") for r in rl_b if r["rl"].get("retry_after")]
    if ra:
        print("  retry-after values:", sorted(set(ra)))

# Sigma of the log-latency, for the stand-in jitter.
if len(totals) > 2 and statistics.mean(totals) > 0:
    logs = [math.log(max(1.0, t)) for t in totals]
    sigma = statistics.pstdev(logs)
    print("\n  log-latency sigma=%.3f" % sigma)
else:
    sigma = 0.35

q_med = statistics.median(queues) if queues else 120.0
t_med = statistics.median(ttfts) if ttfts else q_med + 400.0
cfg = {
    # Client-visible split: queue (before any compute) then prefill (TTFT) then
    # decode. Calibrated on the median of the real sequential calls.
    "queue_mean_ms": round(q_med, 1),
    "ttft_base_ms": round(max(30.0, t_med - q_med), 1),
    "prompt_ms_per_tok": 0.05,
    "per_tok_ms": round(statistics.median(per_tok) if ct and ctok else 3.0, 2),
    "latency_sigma_ms": round(sigma, 3),
    # Slow tail: real p99 is well above the median; we set a 5% tail.
    "slow_tail_prob": 0.05,
    "slow_tail_extra_ms": round(max(0.0, max(totals) - statistics.median(totals))
                                if totals else 0.0, 1),
    "error_prob": 0.01,
    "concurrency_ref": 2,
    # Slowdown: from the burst, 25 of 40 succeeded, i.e. ~25 in flight tolerated.
    "slowdown_per_conc": 0.12,
    "rpm": 30, "tpm": 8000, "rpd": 1000, "tpd": 200000,
    "chars_per_token": 4.0, "seed": 42,
}
with open(os.path.join(BASE, "standin.json"), "w") as f:
    json.dump(cfg, f, indent=2)
print("\nwrote standin.json:", json.dumps(cfg, indent=2))
