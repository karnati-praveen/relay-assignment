"""Recompute every number the report states, from results.csv alone where possible.

This is the independent check: it does not import the report generators. If a
number here disagrees with the prose, the prose is wrong.
"""
import csv, json, statistics
from collections import Counter, defaultdict, OrderedDict
from datetime import datetime, timezone, timedelta

CSV = "results/results.csv"
PRICE_IN, PRICE_OUT = 0.075, 0.30


def rows(path=CSV):
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def pctl(vals, p):
    vs = sorted(float(v) for v in vals if v not in ("", None))
    if not vs:
        return float("nan")
    k = -(-p * len(vs) // 100)
    return vs[k - 1]


def parse_ts(s):
    s = s.strip().replace("Z", "+00:00")
    if "." in s:
        head, rest = s.split(".", 1)
        frac, tz = rest, ""
        for i, ch in enumerate(rest):
            if ch in "+-":
                frac, tz = rest[:i], rest[i:]
                break
        frac = (frac + "000000")[:6]
        s = head + "." + frac + tz
    return datetime.fromisoformat(s)


def peak_queue(rs):
    ev = []
    for r in rs:
        try:
            t = parse_ts(r["start_time"])
            dur = float(r["total_ms"]) / 1000.0
        except Exception:
            continue
        ev.append((t, 1))
        ev.append((t + timedelta(seconds=dur), -1))
    ev.sort(key=lambda x: (x[0], -x[1]))
    cur = best = 0
    for _, d in ev:
        cur += d
        best = max(best, cur)
    return best


R = rows()
print("TOTAL ROWS", len(R))
print()

load = [r for r in R if not r["scenario"].startswith(("quality", "retrieval_"))]
qual = OrderedDict()
for r in R:
    if r["scenario"].startswith(("quality", "retrieval_")):
        qual.setdefault(r["scenario"], []).append(r)

print("=== LOAD (Table 2a/2b) ===")
print(f"{'scenario':18} {'n':>4} {'ok':>3} {'rl':>3} {'to':>3} {'er':>2} "
      f"{'ret':>5} {'tok/ans':>8} {'$/1k':>7} {'peak':>5} "
      f"{'ttft50':>8} {'ttft95':>8} {'ttft99':>8} {'tot50':>8} {'tot95':>8} {'tot99':>8}")
for sc, rs in qual.items():
    pass
byl = OrderedDict()
for r in load:
    byl.setdefault(r["scenario"], []).append(r)
for sc, rs in byl.items():
    ok = [r for r in rs if r["status"] == "ok"]
    n = len(rs)
    rl = sum(1 for r in rs if r["status"] == "rate_limited")
    to = sum(1 for r in rs if r["status"] == "timeout")
    er = sum(1 for r in rs if r["status"] == "error")
    ret = sum(int(r["retries"] or 0) for r in rs)
    tin = sum(int(r["tokens_in"] or 0) for r in rs)
    tout = sum(int(r["tokens_out"] or 0) for r in rs)
    tpa = (tin + tout) / max(1, len(ok))
    cost = ((tin * PRICE_IN + tout * PRICE_OUT) / 1e6) / max(1, len(ok)) * 1000
    ttft = [r["first_token_ms"] for r in ok if float(r["first_token_ms"] or 0) > 0]
    tot = [r["total_ms"] for r in ok]
    print(f"{sc:18} {n:4d} {len(ok):3d} {rl:3d} {to:3d} {er:2d} {ret/n:5.2f} "
          f"{tpa:8.0f} {cost:7.4f} {peak_queue(rs):5d} "
          f"{pctl(ttft,50):8.1f} {pctl(ttft,95):8.1f} {pctl(ttft,99):8.1f} "
          f"{pctl(tot,50):8.1f} {pctl(tot,95):8.1f} {pctl(tot,99):8.1f}")

nload = len(load)
okl = sum(1 for r in load if r["status"] == "ok")
print()
print(f"load rows {nload} | ok {okl} | timeout {sum(1 for r in load if r['status']=='timeout')}"
      f" | rl {sum(1 for r in load if r['status']=='rate_limited')}"
      f" | error {sum(1 for r in load if r['status']=='error')}")
print("retries total", sum(int(r["retries"] or 0) for r in load))
print("giveups + daily", sum(1 for r in load if r["status"] in ("timeout", "rate_limited")))
gv = [float(r["total_ms"]) for r in load if r["status"] == "timeout"]
print(f"giveup total_ms p50 {pctl(gv,50):.1f} p95 {pctl(gv,95):.1f} n {len(gv)}")
daily = [r for r in load if r["status"] == "rate_limited"]
print("daily rate_limited", len(daily), "tokens", sum(int(r["tokens_in"]) + int(r["tokens_out"]) for r in daily))
allok = [r for r in load if r["status"] == "ok"]
tin = sum(int(r["tokens_in"]) for r in allok); tout = sum(int(r["tokens_out"]) for r in allok)
print("load ok tokens/answer", (tin + tout) / len(allok))

print()
print("=== QUALITY (Table 3 + repair) ===")
print(f"{'scenario':16} {'n':>3} {'graded':>6} {'ok':>3} {'corr':>4} {'wrong':>5} {'rw':>2} {'rc':>2} "
      f"{'nm':>2} {'corr%':>6} {'wrong%':>6} {'rc%':>5} {'rw%':>5} {'tok/ans':>7} {'ttft50':>7} {'ttft95':>8} {'tot50':>8} {'tot95':>8}")
for sc, rs in qual.items():
    grades = [r["answer_grade"] for r in rs]
    g = Counter(grades)
    graded = sum(1 for x in grades if x != "not measured")
    ok = [r for r in rs if r["status"] == "ok"]
    tin = sum(int(r["tokens_in"] or 0) for r in rs)
    tout = sum(int(r["tokens_out"] or 0) for r in rs)
    graded_rows = [r for r in rs if r["answer_grade"] != "not measured"]
    tpa = (tin + tout) / max(1, len(graded_rows))
    ttft = [r["first_token_ms"] for r in graded_rows]
    tot = [r["total_ms"] for r in graded_rows]
    c = g["correct"]; w = g["wrong"]; rw = g["refused_wrongly"]; rc = g["refused_correctly"]
    print(f"{sc:16} {len(rs):3d} {graded:6d} {len(ok):3d} {c:4d} {w:5d} {rw:2d} {rc:2d} {g['not measured']:2d} "
          f"{100*c/max(1,graded):6.1f} {100*w/max(1,graded):6.1f} {100*rc/max(1,graded):5.1f} {100*rw/max(1,graded):5.1f} "
          f"{tpa:7.0f} {pctl(ttft,50):7.1f} {pctl(ttft,95):8.1f} {pctl(tot,50):8.1f} {pctl(tot,95):8.1f}")

# by setting within quality_sweep
print()
for setting in ("p220_k6_ryes", "p120_k4_rno", "p450_k3_ryes"):
    rs = [r for r in qual.get("quality_sweep", []) if r["retrieval_setting"] == setting]
    if not rs:
        continue
    g = Counter(r["answer_grade"] for r in rs)
    graded = len(rs) - g["not measured"]
    graded_rows = [r for r in rs if r["answer_grade"] != "not measured"]
    ttft = [r["first_token_ms"] for r in graded_rows]
    tot = [r["total_ms"] for r in graded_rows]
    tin = sum(int(r["tokens_in"]) for r in rs); tout = sum(int(r["tokens_out"]) for r in rs)
    print(f"{setting}: n={len(rs)} graded={graded} grades={dict(g)} "
          f"tok/ans={(tin+tout)/graded:.0f} ttft50={pctl(ttft,50):.1f} ttft95={pctl(ttft,95):.1f} "
          f"tot50={pctl(tot,50):.1f} tot95={pctl(tot,95):.1f}")
    print("   per-q grades:", [(r["request_id"].split("-")[-1], r["answer_grade"]) for r in rs])

for setting in ("p220_k6_rno", "p900_k2_ryes"):
    rs = [r for r in qual.get("retrieval_standin", []) if r["retrieval_setting"] == setting]
    if not rs:
        continue
    ttft = [r["first_token_ms"] for r in rs]
    tot = [r["total_ms"] for r in rs]
    tin = sum(int(r["tokens_in"]) for r in rs); tout = sum(int(r["tokens_out"]) for r in rs)
    print(f"{setting} (stand-in): n={len(rs)} tok/ans={(tin+tout)/len(rs):.0f} "
          f"ttft50={pctl(ttft,50):.1f} ttft95={pctl(ttft,95):.1f} "
          f"tot50={pctl(tot,50):.1f} tot95={pctl(tot,95):.1f}")

print()
print("=== RECALL (retrieval_recall.detail.jsonl) ===")
try:
    rec = [json.loads(l) for l in open("results/retrieval_recall.detail.jsonl", encoding="utf-8") if l.strip()]
    by = defaultdict(list)
    for r in rec:
        by[r.get("setting")].append(r)
    for s, rs in by.items():
        hit = sum(1 for r in rs if r.get("gold_hit"))
        print(f"{s}: {hit}/{len(rs)}")
except FileNotFoundError:
    print("missing")

print()
print("=== SECTION 4 SLOW REQUEST ===")
ld = [json.loads(l) for l in open("results/load_detail.jsonl", encoding="utf-8") if l.strip()]
pool = [d for d in ld if d.get("scenario") == "steady" and d.get("status") == "ok"
        and float(d.get("first_token_ms") or 0) > 0]
pick = max(pool, key=lambda d: float(d["total_ms"]))
tot = float(pick["total_ms"]); wait = float(pick["waiting_ms"])
ret = float(pick["retrying_ms"]); work = float(pick["working_ms"])
ttft = float(pick["first_token_ms"])
print(pick.get("request_id"), "total", tot, "wait", wait, round(100*wait/tot, 1),
      "retry", ret, round(100*ret/tot, 1), "work", work, round(100*work/tot, 1),
      "ttft", ttft, "ttft%", round(100*ttft/tot, 1), "sum", wait+ret+work)

print()
print("=== SECTION 7 REPLY ===")
rep = [json.loads(l) for l in open("results/daily_exhausted_reply.jsonl", encoding="utf-8") if l.strip()]
lat = [float(r.get("ms") or r.get("total_ms") or 0) for r in rep]
print("n", len(rep), "ms", lat, "median", statistics.median(lat), "min", min(lat), "max", max(lat))
for r in rep[:1]:
    print(json.dumps(r, ensure_ascii=False)[:1200])

print()
print("=== TABLE 1 EVIDENCE ===")
for path in ("results/real_limits_probe.jsonl", "results/real_probe.jsonl"):
    for line in open(path, encoding="utf-8"):
        d = json.loads(line)
        print(path, json.dumps(d, ensure_ascii=False)[:500])
