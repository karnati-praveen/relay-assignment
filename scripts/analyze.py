"""Compute every required table from the raw results, exactly, so a reviewer can
recompute them. Prints markdown to stdout and writes results/tables.md.

Inputs:
  results/results.csv          load scenarios (Tables 1 evidence, 2a, 2b)
  results/scenario_meta.json   scenario parameters (duration, offered rate, peak queue)
  results/quality_real.csv      Table 3, real service quality
  results/retrieval_standin.csv Table 3, stand-in latency
  results/quality_real.detail.jsonl per-question detail incl. the timing split
  results/real_findings.md      Table 1 evidence (hand-written from raw probes)
"""
import csv, json, os, statistics
from collections import OrderedDict

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
R = os.path.join(BASE, "results")

sys_path_scripts = os.path.join(BASE, "scripts")
if sys_path_scripts not in __import__("sys").path:
    __import__("sys").path.insert(0, sys_path_scripts)
import sweep_stats

# gpt-oss-20b list price (USD per 1M tokens) for the cost column only. This is a
# notional figure to show the shape of the cost; the free tier itself is $0.
PRICE_IN, PRICE_OUT = 0.075, 0.30


def read_csv(path):
    if not os.path.exists(path):
        return []
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def read_jsonl(path):
    out = []
    if not os.path.exists(path):
        return out
    for line in open(path, encoding="utf-8"):
        line = line.strip()
        if line:
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def pctl(values, p):
    """Nearest-rank percentile: the ceil(p/100 * n)-th smallest value.

    Every published percentile is therefore an actually observed request, and
    the rule is one line, so a reviewer can recompute it from results.csv:
    sort the answered requests, take item ceil(p*n/100) counting from 1.
    """
    vs = sorted(v for v in values if v is not None)
    if not vs:
        return float("nan")
    k = int(-(-p * len(vs) // 100))       # ceil, integer arithmetic
    return vs[min(max(k, 1), len(vs)) - 1]


def n1(x):
    if x != x:
        return "n/a"
    return f"{x:.1f}"


def fl(x, nd=1):
    try:
        return f"{float(x):.{nd}f}"
    except (TypeError, ValueError):
        return "n/a"


def peak_in_flight(rs):
    """Peak concurrent requests from results.csv (start_time + total_ms)."""
    from datetime import datetime
    ev = []
    for r in rs:
        try:
            st = datetime.fromisoformat(r["start_time"].replace("Z", "+00:00")).timestamp()
            en = st + float(r["total_ms"]) / 1000.0
            ev.append((st, 1))
            ev.append((en, -1))
        except Exception:
            continue
    ev.sort(key=lambda x: (x[0], -x[1]))
    cur = pk = 0
    for _, d in ev:
        cur += d
        if cur > pk:
            pk = cur
    return pk if ev else "not measured"


def scenario_meta():
    p = os.path.join(R, "scenario_meta.json")
    return json.load(open(p)) if os.path.exists(p) else {}


def table_1():
    """Table 1: the real service's limits, in the brief's exact columns."""
    print("## Table 1: Limits you found in the real service\n")
    print("| Limit | Value you found | How you found it | Confidence (high, medium, low) |")
    print("|---|---|---|---|")
    rows = [
        ("Requests per minute", "30",
         "429 body: `Limit 30, Used 30, Requested 1` "
         "(`results/real_burst.jsonl`)", "high"),
        ("Tokens per minute", "8,000",
         "`rl.limit_tokens` (response header) in "
         "`results/real_limits_probe.jsonl`", "high"),
        ("Requests per day", "1,000",
         "`rl.limit_requests` (response header), same file", "high"),
        ("Tokens per day", "200,000",
         "429 body: `Limit 200000, Used 199927, Requested 153` "
         "(`results/real_probe.jsonl`)", "high"),
        ("Requests in flight at once", "~25",
         "40 fired at once → 25 ok, 15 rate-limited (`results/real_burst.jsonl`)",
         "medium"),
        ("What it returns when a limit is hit",
         "HTTP 429 + `retry-after`; JSON `type` `\"requests\"` or `\"tokens\"`",
         "captured 429 bodies", "high"),
        ("Time until it recovers",
         "RPM `retry-after: 2 s`; the daily-cap 429 asked 34.56 s; "
         "the day bucket refills at 2.31 tokens/s",
         "`results/real_burst.jsonl`, `results/real_probe.jsonl`", "high"),
    ]
    for a, b, c, d in rows:
        print(f"| {a} | {b} | {c} | {d} |")
    print()


SCENARIO_LABEL = {
    "sustainable": "sustainable (capacity)",
    "steady": "steady",
    "burst": "burst",
    "overload": "sustained overload",
    "rpm_half": "per-minute limit halved",
    "daily_exhausted": "daily budget exhausted",
}


def load_rows(rows):
    """Tables 2a/2b are the six *load shapes* only.

    results.csv also carries the real-service quality sweeps (`quality_sweep`,
    `quality_fix20`) and the stand-in retrieval sweep (`retrieval_standin`) so
    every Table 3 row recomputes from the one raw file, but those sweeps are
    not load shapes and must not appear in Tables 2a/2b.
    """
    return [r for r in rows if r["scenario"] in SCENARIO_LABEL]


TABLE_2A_HEADER = ("| Scenario | Real or stand-in | Requests sent | "
                   "Duration (s) | Offered rate (per min) | First token p50 / "
                   "p95 / p99 (ms) | Total time p50 / p95 / p99 (ms) |")
TABLE_2B_HEADER = ("| Scenario | Succeeded % | Rate-limited % | Timed out or "
                   "given up % | Retries per request | Peak queue length | "
                   "Tokens per answer | Cost per 1,000 answers |")


def table_2a_rows(rows, meta):
    """Table 2a's data rows, shared with finalize_report.py."""
    out = []
    by = OrderedDict()
    for r in rows:
        by.setdefault(r["scenario"], []).append(r)
    for sc, rs in by.items():
        m = meta.get(sc, {})
        # Latency is the latency of an answer. A request that was shed in
        # 0.2 ms produced no first token and no answer, so mixing it into the
        # percentiles would make a mostly-refused scenario look fast. Outcome
        # counts (including sheds) are in Table 2b.
        answered = [x for x in rs if x["status"] == "ok"]
        ttft = [float(x["first_token_ms"]) for x in answered
                if x["first_token_ms"] and float(x["first_token_ms"]) > 0]
        tot = [float(x["total_ms"]) for x in answered]
        rate = m.get("offered_rate", "n/a")
        if sc == "burst" and m.get("burst_count"):
            rate = "%s, then +%s in one burst" % (rate, m["burst_count"])
        out.append("| %s | %s | %d (%d ok) | %s | %s | %s / %s / %s | %s / %s / %s |" % (
            SCENARIO_LABEL.get(sc, sc),
            "real" if rs[0]["source"] == "real" else "stand-in",
            len(rs), len(answered), m.get("duration_s", "n/a"), rate,
            n1(pctl(ttft, 50)), n1(pctl(ttft, 95)), n1(pctl(ttft, 99)),
            n1(pctl(tot, 50)), n1(pctl(tot, 95)), n1(pctl(tot, 99))))
    return out


def table_2b_rows(rows, meta):
    """Table 2b's data rows, shared with finalize_report.py."""
    out = []
    by = OrderedDict()
    for r in rows:
        by.setdefault(r["scenario"], []).append(r)
    for sc, rs in by.items():
        n = len(rs)
        if n == 0:
            continue
        ok = sum(1 for x in rs if x["status"] == "ok")
        rl = sum(1 for x in rs if x["status"] == "rate_limited")
        # `busy` is written as `timeout` in results.csv (the brief's only
        # vocabulary for a give-up); both mean our admission control refused
        # before a provider call, which is what this column counts.
        er = sum(1 for x in rs if x["status"] in ("error", "timeout", "busy"))
        retries = sum(int(x["retries"] or 0) for x in rs)
        tin = sum(int(x["tokens_in"] or 0) for x in rs)
        tout = sum(int(x["tokens_out"] or 0) for x in rs)
        # Both token figures count every token the scenario spent and divide by
        # the answers it actually produced. The tokens burned by a request that
        # was later shed are part of the cost of the answers that succeeded, so
        # dividing by n (all requests) would hide exactly the waste this
        # scenario exists to measure.
        tok_per = (tin + tout) / max(1, ok)
        # Cost per 1,000 answers at list price; only successful requests
        # produced an answer, so cost normalises on the answers actually given.
        cost = ((tin * PRICE_IN + tout * PRICE_OUT) / 1e6) / max(1, ok) * 1000
        peak = meta.get(sc, {}).get("peak_queue", peak_in_flight(rs))
        out.append("| %s | %.1f | %.1f | %.1f | %.2f | %s | %.0f | $%.4f |" % (
            SCENARIO_LABEL.get(sc, sc), 100 * ok / n, 100 * rl / n, 100 * er / n,
            retries / n, peak, tok_per, cost))
    return out


def table_2a(rows, meta):
    print("## Table 2a: Latency, one row per load scenario\n")
    print(TABLE_2A_HEADER)
    print("|---|---|---|---|---|---|---|")
    for line in table_2a_rows(rows, meta):
        print(line)
    print("\nLatency cells cover only requests that produced an answer "
          "(`status=ok`); the `n ok` beside `Requests sent` is that cell's "
          "sample size. Percentiles are nearest-rank: item ceil(p*n/100), "
          "counting from 1, of the sorted answered requests, so every cell is "
          "an observed request and recomputes from results.csv. With n ok < 20 "
          "p95 and p99 are the same request; with n ok = 1 all three are. The "
          "shed/refused requests are counted in Table 2b.\n")


def table_2b(rows, meta):
    print("## Table 2b: Outcomes, same rows\n")
    print(TABLE_2B_HEADER)
    print("|---|---|---|---|---|---|---|---|")
    for line in table_2b_rows(rows, meta):
        print(line)
    print("\n`Rate-limited` is the provider's own 429. `Timed out or given up` "
          "includes the `timeout` rows that our admission control refused "
          "before spending a provider call, in milliseconds, rather than "
          "queueing them past the scenario. `Tokens per answer` and `Cost per "
          "1,000 answers` count every token the scenario spent, divided by the "
          "answers it actually delivered.\n")


def table_3():
    print("## Table 3: Retrieval settings (at least three)\n")
    print("Quality columns need the real service; the stand-in returns canned "
          "text, so its quality cells are \"not measured\". Tokens and latency "
          "are comparable across both. The two comparison settings use the "
          "first ten questions (q01–q10); the chosen setting uses all twenty.\n")
    print("| Setting (piece size, pieces fetched, re-rank yes or no) | "
          "Questions asked | Correct and cited % | Wrong % | Correctly refused % | "
          "Tokens per answer | First token p50 / p95 (ms) | Total time p50 / p95 (ms) |")
    print("|---|---|---|---|---|---|---|---|")
    for s in sweep_stats.table3_rows(R):
        if s["source"] == "real":
            nm = " **(real)**"
            quality = [fl(s["correct_pct"]), fl(s["wrong_pct"]),
                       fl(s["refused_correct_pct"])]
        else:
            nm = " (stand-in)"
            quality = ["not measured"] * 3
        qcell = (str(s["asked"]) if s["asked"] == s["evaluated"]
                 else "%d (%d graded)" % (s["asked"], s["evaluated"]))
        cells = [s["setting"] + nm, qcell] + quality + [
            fl(s["tokens_per_answer"], 0),
            "%s / %s" % (fl(s["ttft_p50"], 1), fl(s["ttft_p95"], 1)),
            "%s / %s" % (fl(s["total_p50"], 1), fl(s["total_p95"], 1)),
        ]
        print("| " + " | ".join(cells) + " |")
    print()


def retrieval_recall():
    """Retrieval quality with no model in the loop.

    Table 3's quality columns need the real service, and the stand-in cannot
    judge an answer. Recall can: for each answerable question, is the sentence
    that answers it present in the context a setting returns? Routing is held
    at the question's own paper, so the only variable is retrieval.
    """
    rows = read_csv(os.path.join(R, "retrieval_recall.csv"))
    if not rows:
        return
    print("## Table 3b: Retrieval recall (offline, no model call)\n")
    print("| Setting | Answerable questions | Gold sentence in context % | "
          "Context words (mean) | Pieces (mean) |")
    print("|---|---|---|---|---|")
    for r in rows:
        print("| %s | %s | %s | %s | %s |" % (
            r.get("setting", "?"), r.get("questions", "?"),
            fl(r.get("recall_pct")), fl(r.get("context_words_mean"), 0),
            fl(r.get("pieces_mean"), 2)))
    print("\n`Gold sentence in context` is a property of retrieval alone: it "
          "does not say the model used the sentence, only that the setting "
          "retrieved it. The patterns are hand-written from the pinned text "
          "(`gold` in `data/questions.json`).\n")


def timing_split():
    """Where the wall time went, from the real-service detail file.

    The detail records ttft_ms and total_ms for every question, plus the
    pacing wait the harness spent earning that question's tokens (pacing_ms,
    deliberately outside the request's total_ms). ttft/total is the share of a
    request that elapsed before its first streamed token.
    """
    all_det = read_jsonl(os.path.join(R, "quality_real.detail.jsonl"))
    if not all_det:
        print("## Timing split\n\n(no detail yet)\n")
        return
    # Use the same evaluated set as Table 3: hand-verified when the manual file
    # exists, otherwise the sweep's own automatic verdict.
    manual = sweep_stats.load_manual(os.path.join(R, "quality_manual.csv"))
    if manual:
        det = [d for d in all_det
               if manual.get(d["qid"], {}).get("verdict") != "not_measured"]
    else:
        det = [d for d in all_det if d.get("grade_auto") != "not_measured"]
    print("## Where the time goes (real service, chosen setting)\n")
    print("| Metric | p50 | p95 | max |")
    print("|---|---|---|---|")
    tot = [float(d["total_ms"]) for d in det]
    ttf = [float(d["ttft_ms"]) for d in det]
    tok = [int(d.get("tokens_in", 0)) + int(d.get("tokens_out", 0)) for d in det]
    for name, vals in [("Total (ms)", tot), ("First token (ms)", ttf),
                       ("Tokens/answer (graded row)", tok)]:
        print("| %s | %s | %s | %s |" % (name, n1(pctl(vals, 50)),
                                         n1(pctl(vals, 95)), n1(max(vals))))
    med_ttft = pctl(ttf, 50)
    med_tot = pctl(tot, 50)
    print("\n- Median first-token share of total: **%.0f%%** "
          "(%.0f of %.0f ms) — i.e. most of a slow answer is spent *before* "
          "any token arrives, which is waiting, not model work."
          % (100 * med_ttft / max(1.0, med_tot), med_ttft, med_tot))
    n = len(det)
    tin = sum(int(d.get("tokens_in", 0)) for d in all_det)
    tout = sum(int(d.get("tokens_out", 0)) for d in all_det)
    print("- Questions measured: %d (of %d asked)" % (n, len(all_det)))
    print("- Tokens per answer (all sweep spend / %d graded): **%.0f** (in %d, out %d)" %
          (n, (tin + tout) / n, tin / n, tout / n))
    pace = [float(d.get("pacing_ms", 0.0)) for d in all_det if float(d.get("pacing_ms", 0.0)) > 0]
    if pace:
        print("- Median pacing wait before a question (outside its total): "
              "**%.0f s**" % (pctl(pace, 50) / 1000.0))
    print()


def answer_quality():
    det = read_jsonl(os.path.join(R, "quality_real.detail.jsonl"))
    if not det:
        return
    # Only questions that reached a final verdict carry a model judgement.
    # Operational failures (daily budget or checker JSON exhaustion) are
    # reported as not measured, never as correct or wrong.
    ne = [d for d in det if d.get("grade_auto") != "not_measured"]
    nm = [d for d in det if d.get("grade_auto") == "not_measured"]
    err_rows = [d for d in det if d.get("status") == "error"]
    print("## Answer quality (real service)\n")
    print("Automatic citation grade (`grade_auto` in `quality_real.detail.jsonl` "
          "— checks only that a citation parsed; `results.csv` carries the "
          "hand-verified grade below):\n")
    print("| Grade | Count | % of evaluated |")
    print("|---|---|---|")
    total = len(ne)
    for g in ["correct", "wrong", "refused_correctly", "refused_wrongly"]:
        c = sum(1 for d in ne if d.get("grade_auto") == g)
        pct = (100 * c / total) if total else 0.0
        print("| %s | %d | %.1f |" % (g, c, pct))
    print("| not measured (daily budget stopped) | %d | — |" % len(nm))
    err_note = (" (`%s` had `status=error` after checker JSON retry exhaustion, "
                "counted as `refused_wrongly` by `grade_auto` and reclassified "
                "`not measured` in hand grading)" % ", ".join(d["qid"] for d in err_rows)
                if err_rows else "")
    print("\n- Evaluated by `grade_auto`: **%d of %d**%s." % (total, len(det), err_note))
    if nm:
        print("- Stopped by the provider's daily token budget: **%d** — these "
              "are operational stoppages, not model judgements." % len(nm))
    print("\nThe automatic grade is deliberately coarse: it cannot tell a right "
          "fact from a wrong one, and it treats any cited answer as correct. "
          "The hand-verified fact grade is reconciled separately in "
          "`results/quality_manual.md`; `results/results.csv`, Table 3, and the "
          "report use that hand-verified grade over the 19 evaluated questions.\n")

    # The reconciliation itself: where the citation-only grade and the fact
    # grade disagree is the most useful number in the quality section.
    manual = sweep_stats.load_manual(os.path.join(R, "quality_manual.csv"))
    if not manual:
        print("> Hand-verified verdicts are missing "
              "(`results/quality_manual.csv`); Table 3 is provisional.\n")
        return
    graded = [d for d in det if manual[d["qid"]]["verdict"] != "not_measured"]
    print("### Hand-verified fact grade (Table 3 and `results.csv` use this)\n")
    print("| Verdict | Count | %% of %d graded |" % len(graded))
    print("|---|---|---|")
    for g in ["correct", "wrong", "refused_correctly", "refused_wrongly"]:
        c = sum(1 for d in graded if manual[d["qid"]]["verdict"] == g)
        print("| %s | %d | %.1f |" % (g, c, 100.0 * c / len(graded)))
    stop = [d for d in det if manual[d["qid"]]["verdict"] == "not_measured"]
    if stop:
        reasons = ", ".join("%s: status=%s" % (d["qid"], d.get("status", "?")) for d in stop)
        print("| not measured (unanswered operational error — %s) | %d | — |" % (reasons, len(stop)))
    diff = [(d["qid"], d["grade_auto"], manual[d["qid"]]["verdict"])
            for d in det
            if manual[d["qid"]]["verdict"] != "not_measured"
            and d.get("grade_auto") != manual[d["qid"]]["verdict"]]
    print("\n- Automatic and hand grades disagree on **%d of %d** rows%s. "
          "A cited answer is not automatically a right answer; the table above "
          "is the one the report quotes."
          % (len(diff), len(graded),
             (": " + ", ".join("%s auto=%s manual=%s" % t for t in diff)) if diff else ""))
    pages_ok = sum(1 for d in graded if manual[d["qid"]].get("page_ok") == "yes")
    print("- Citation page is the best page for the fact: **%d of %d**."
          % (pages_ok, len(graded)))
    print()


def main():
    rows = read_csv(os.path.join(R, "results.csv"))
    meta = scenario_meta()
    out = []
    import io
    buf = io.StringIO()
    global _stdout
    # print to both stdout and a buffer
    def emit():
        pass
    # simple approach: capture by re-running the printers into a buffer
    import contextlib
    with contextlib.redirect_stdout(buf):
        if rows:
            table_1()
            table_2a(load_rows(rows), meta)
            table_2b(load_rows(rows), meta)
        else:
            print("## Tables 2a/2b\n\n(no results/results.csv yet)\n")
        table_3()
        retrieval_recall()
        timing_split()
        answer_quality()
    text = buf.getvalue()
    print(text)
    open(os.path.join(R, "tables.md"), "w", encoding="utf-8").write(text)
    print("wrote results/tables.md")


if __name__ == "__main__":
    main()
