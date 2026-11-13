"""Capture the stand-in behaviours that the load run cannot show, as evidence.

The brief asks the stand-in to be slow, to fail, to refuse when a limit is
reached, to slow down under load, and to stream token by token.

- slow / slow under load / refuse on limit: already in results/load_results.csv
  (status=busy|rate_limited under five offered shapes), and asserted below.
- fails: error_prob=1 makes every call a 500; this file records the retries and
  the refusal that follow, so the error path is not just unit-tested.
- streams: a normal call records the per-call SSE event count, and a first
  token far before the total.

Output: results/standin_faults.jsonl, one JSON object per behaviour, with the
per-call trace and the config knob that produced it.
"""
import json
import os
import subprocess
import sys
import time
import csv

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(BASE, "bin")
OUT = os.path.join(BASE, "results", "standin_faults.jsonl")
LOAD = os.path.join(BASE, "results", "load_results.csv")

Q = "What is the dimension of the model in the base Transformer?"

BASE_CFG = {
    "queue_mean_ms": 314.4, "ttft_base_ms": 79.1, "prompt_ms_per_tok": 0.05,
    "per_tok_ms": 1.11, "latency_sigma_ms": 0.084, "slow_tail_prob": 0.05,
    "slow_tail_extra_ms": 109.5, "error_prob": 0.01, "concurrency_ref": 2,
    "slowdown_per_conc": 0.12, "rpm": 30, "tpm": 8000, "rpd": 1000,
    "tpd": 200000, "chars_per_token": 4.0, "seed": 42,
}


def cfg(**over):
    c = dict(BASE_CFG)
    c.update(over)
    path = os.path.join(BASE, "results", "tmp_fault_cfg.json")
    with open(path, "w", encoding="utf-8") as f:
        json.dump(c, f, indent=2)
    return path, c


def start(cfg_path, port):
    p = subprocess.Popen(
        [os.path.join(BIN, "standin.exe"), "-addr", "127.0.0.1:%d" % port,
         "-config", cfg_path],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1.5)
    return p


def ask(port):
    t0 = time.time()
    r = subprocess.run(
        [os.path.join(BIN, "relay.exe"), "ask", "-source", "stand-in",
         "-base", "http://127.0.0.1:%d/openai/v1" % port,
         "-limits=false", "-v", "-q", Q],
        capture_output=True, timeout=180)
    text = r.stdout.decode("utf-8", "replace")
    i = text.find("{")
    if i < 0:
        raise SystemExit("no trace in output: " + text[:500])
    trace = json.loads(text[i:])
    trace["_wall_ms"] = (time.time() - t0) * 1000.0
    return trace


def row(behaviour, trace, **extra):
    d = {"behaviour": behaviour}
    d.update(extra)
    d["refused"] = trace.get("refused")
    d["retries"] = trace.get("retries")
    firsts = [c.get("ttft_ms") or 0 for c in trace.get("calls") or []
              if c.get("status") == "ok" and (c.get("ttft_ms") or 0) > 0]
    d["first_token_ms"] = min(firsts) if firsts else 0
    d["wall_ms"] = round(trace.get("_wall_ms") or 0, 1)
    d["tokens_in"] = trace.get("tokens_in")
    d["tokens_out"] = trace.get("tokens_out")
    d["calls"] = [
        {"step": c.get("step"), "attempt": c.get("attempt"),
         "status": c.get("status"), "stream_events": c.get("stream_events"),
         "ttft_ms": c.get("ttft_ms"),
         "tokens_in": c.get("tokens_in"), "tokens_out": c.get("tokens_out"),
         "error": (c.get("error") or "")[:200]}
        for c in trace.get("calls") or []]
    return d


# The load run must already show the limit path; refuse to write this file if
# it does not, so the citation in the report cannot go stale silently.
with open(LOAD, newline="", encoding="utf-8") as f:
    load = list(csv.DictReader(f))
limited = sum(1 for r in load if r["status"] == "rate_limited")
busy = sum(1 for r in load if r["status"] == "busy")
errs = sum(1 for r in load if int(r["retries"] or 0))
print("load: %d rows, %d rate_limited, %d busy, %d retried" %
      (len(load), limited, busy, errs), flush=True)
assert limited > 0 and busy > 0, "load_results.csv no longer shows the limit path"
assert errs == 0, "load run now contains retries; section 6's claim changed"

rows = []

# 1. Fails: every call 500s. Limits raised so this is purely the error path.
path, _ = cfg(error_prob=1.0, rpm=100000, tpm=100000000,
              rpd=1000000, tpd=100000000)
p = start(path, 8111)
try:
    t = ask(8111)
    rows.append(row("error_500_every_call", t, config={"error_prob": 1.0},
                    note="every call 500s; pipeline retries, then refuses"))
    print("error row done", flush=True)
finally:
    p.terminate()

# 2. Streams: a normal call. Many SSE events per call; first token before total.
path, _ = cfg(error_prob=0.0)
p = start(path, 8113)
try:
    t = ask(8113)
    rows.append(row("stream_token_by_token", t, config={"error_prob": 0.0},
                    note="per-call SSE event count; ttft << total"))
    print("stream row done", flush=True)
finally:
    p.terminate()

with open(OUT, "w", encoding="utf-8", newline="\n") as f:
    for d in rows:
        f.write(json.dumps(d, sort_keys=True) + "\n")

tmp_cfg = os.path.join(BASE, "results", "tmp_fault_cfg.json")
if os.path.exists(tmp_cfg):
    os.remove(tmp_cfg)

for d in rows:
    ev = sum(c["stream_events"] or 0 for c in d["calls"] or [])
    print("%-28s refused=%-5s retries=%d first_token=%sms wall=%sms events=%d calls=%d" % (
        d["behaviour"], d["refused"], d["retries"] or 0,
        round(d["first_token_ms"] or 0), d["wall_ms"], ev, len(d["calls"])))
print("wrote", OUT)
