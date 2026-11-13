"""Measure the stand-in's slowdown-under-load term directly.

The load shapes in results/load_results.csv are shaped by admission control:
above ~1.3 requests/minute the token budget admits one request per 47 s, so a
saturated shape completes one answer and sheds the rest. That is the right
finding for the system, but it means Table 2a cannot show the stand-in's own
"slower under load" behaviour, because the pipeline never lets two requests be
in flight at once.

This probe removes the limit gate (limits raised via flags) and fires the same
single call at 1, 2, 4, 8, 16 and 32 concurrent, three rounds each. What is
left is the load term in cmd/standin's latency model:

    mult = 1 + max(0, concurrency - concurrency_ref) * slowdown_per_conc

Output: results/standin_slowdown.csv, one row per request, and a median
per concurrency on stdout. The limits themselves are measured elsewhere
(results/real_limits_probe.jsonl); raising them here is deliberate, to isolate
the load term, and this file is the record of how the numbers were produced.
"""
import csv
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(BASE, "bin", "standin.exe")
CFG = os.path.join(BASE, "standin.json")
OUT = os.path.join(BASE, "results", "standin_slowdown.csv")
PORT = 8098
ROUNDS = 3
CONCURRENCY = (1, 2, 4, 8, 16, 32)

# A prompt of roughly the size the real pipeline sends to one call
# (~2,300 tokens at 4 chars/token), so prefill is not the toy case.
FILLER = (
    "The paper describes an encoder-decoder architecture built from stacked "
    "self-attention and feed-forward sub-layers, with residual connections and "
    "layer normalisation around each sub-layer, and it reports machine "
    "translation results on WMT 2014 English-to-German and English-to-French. "
)
PROMPT = "Summarise the method.\n\n" + FILLER * 60


def start():
    p = subprocess.Popen(
        [BIN, "-addr", "127.0.0.1:%d" % PORT, "-config", CFG,
         "-rpm", "1000000", "-tpm", "1000000000",
         "-rpd", "1000000", "-tpd", "1000000000"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1.5)
    return p


def one(conc, rnd):
    body = json.dumps({
        "model": "stand-in",
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": True,
    }).encode()
    req = urllib.request.Request(
        "http://127.0.0.1:%d/openai/v1/chat/completions" % PORT, data=body,
        headers={"Content-Type": "application/json", "Authorization": "Bearer x"})
    t0 = time.perf_counter()
    ttft = None
    status = "ok"
    out_tokens = 0
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                try:
                    chunk = json.loads(payload)
                except ValueError:
                    continue
                for ch in chunk.get("choices") or []:
                    if (ch.get("delta") or {}).get("content"):
                        if ttft is None:
                            ttft = (time.perf_counter() - t0) * 1000.0
                        out_tokens += 1
    except urllib.error.HTTPError as e:
        status = "http_%d" % e.code
    except Exception as e:  # noqa: BLE001 - recorded, not raised
        status = "error"
        print("  %s: %s" % (status, e), file=sys.stderr)
    total = (time.perf_counter() - t0) * 1000.0
    return {"concurrency": conc, "round": rnd, "status": status,
            "ttft_ms": round(ttft, 1) if ttft is not None else "",
            "total_ms": round(total, 1), "tokens_out": out_tokens}


def fire(conc, rnd):
    out = []
    lock = threading.Lock()
    barrier = threading.Barrier(conc)

    def worker(i):
        barrier.wait()
        row = one(conc, rnd)
        with lock:
            out.append(row)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(conc)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    return out


def pct(xs, p):
    if not xs:
        return ""
    xs = sorted(xs)
    k = max(1, -(-len(xs) * p // 100))
    return xs[k - 1]


def main():
    p = start()
    rows = []
    try:
        for conc in CONCURRENCY:
            for rnd in range(ROUNDS):
                rows.extend(fire(conc, rnd))
            got = [r for r in rows if r["concurrency"] == conc and r["status"] == "ok"]
            if not got:
                print("c=%-2d n=0" % conc)
                continue
            print("c=%-2d n=%-2d ttft p50 %6.1f ms   total p50 %6.1f  p95 %6.1f ms"
                  % (conc, len(got), pct([r["ttft_ms"] for r in got], 50),
                     pct([r["total_ms"] for r in got], 50),
                     pct([r["total_ms"] for r in got], 95)))
    finally:
        p.terminate()
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()

    with open(OUT, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=["concurrency", "round", "status",
                                          "ttft_ms", "total_ms", "tokens_out"])
        w.writeheader()
        w.writerows(rows)
    print("wrote %s (%d rows)" % (OUT, len(rows)))


if __name__ == "__main__":
    main()
