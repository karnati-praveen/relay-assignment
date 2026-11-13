"""Capture the 3pm reply: daily budget scaled to zero.

Section 7 claims the caller gets the quota reply immediately, with no model
call. That is a measurement, so it is captured here rather than asserted:
`-tpd 1` makes the pipeline's own day bucket empty before any provider call,
and five runs give the latency distribution.

Output: results/daily_exhausted_reply.jsonl
"""
import json
import os
import re
import statistics
import subprocess

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(BASE, "bin", "relay.exe")
Q = "What is the dimension of the model in the base Transformer?"
OUT = os.path.join(BASE, "results", "daily_exhausted_reply.jsonl")

rows = []
for i in range(5):
    r = subprocess.run(
        [BIN, "ask", "-source", "stand-in",
         "-base", "http://127.0.0.1:8114/openai/v1",
         "-tpd", "1", "-q", Q],
        capture_output=True, timeout=120)
    text = r.stdout.decode("utf-8", "replace")
    m = re.search(r"refused=(\w+) ttft_ms=([\d.]+) total_ms=([\d.]+) citations=(\d+) "
                  r"retries=(\d+) tokens_in=(\d+) tokens_out=(\d+)", text)
    if not m:
        raise SystemExit("could not parse output:\n" + text[:1000])
    answer = ""
    a = text.find("--- ANSWER ---")
    b = text.find("--- META ---")
    if a >= 0 and b > a:
        answer = text[a + len("--- ANSWER ---"):b].strip()
    rows.append({
        "run": i, "refused": m.group(1) == "true",
        "ttft_ms": float(m.group(2)), "total_ms": float(m.group(3)),
        "citations": int(m.group(4)), "retries": int(m.group(5)),
        "tokens_in": int(m.group(6)), "tokens_out": int(m.group(7)),
        "answer": answer,
    })

with open(OUT, "w", encoding="utf-8", newline="\n") as f:
    for d in rows:
        f.write(json.dumps(d, sort_keys=True) + "\n")

ms = [d["total_ms"] for d in rows]
print("runs %d | median %.1f ms | min %.1f max %.1f | tokens %d" % (
    len(rows), statistics.median(ms), min(ms), max(ms),
    sum(d["tokens_in"] + d["tokens_out"] for d in rows)))
print("refused all:", all(d["refused"] for d in rows),
      "| citations:", rows[0]["citations"])
print("answer:", rows[0]["answer"][:120].replace("\n", " "))
print("wrote", OUT)
