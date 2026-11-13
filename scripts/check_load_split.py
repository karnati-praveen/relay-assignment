"""Check that every answered load request's time splits exactly into
waiting + retrying + working.

Section 4 quotes one request's split, and the claim that the three buckets are
disjoint and complete is only worth making if it holds for every row, not just
the one that gets quoted. Run after scripts/run_load.ps1:

    python scripts/check_load_split.py

Exits non-zero (and prints the offenders) if any `ok` row does not add up
within a millisecond.
"""
import json
import os
import sys

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DETAIL = os.path.join(BASE, "results", "load_detail.jsonl")
TOL_MS = 1.0

rows = [json.loads(l) for l in open(DETAIL, encoding="utf-8") if l.strip()]
ok = [d for d in rows if d.get("status") == "ok"]
bad = []
for d in ok:
    tot = float(d["total_ms"])
    parts = (float(d.get("waiting_ms", 0.0)) + float(d.get("retrying_ms", 0.0))
             + float(d.get("working_ms", 0.0)))
    if abs(tot - parts) > TOL_MS:
        bad.append((d.get("request_id"), tot, parts))

print("rows %d | ok %d | splits checked %d | mismatches %d"
      % (len(rows), len(ok), len(ok), len(bad)))
for rid, tot, parts in bad[:20]:
    print("  %s total=%.3f sum=%.3f diff=%+.3f" % (rid, tot, parts, parts - tot))
if bad:
    sys.exit("waiting+retrying+working does not equal total for %d answered rows"
             % len(bad))
print("every answered request splits into waiting + retrying + working")
