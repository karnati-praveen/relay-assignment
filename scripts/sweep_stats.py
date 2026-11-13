"""One definition of the real sweep's numbers, shared by analyze.py and
finalize_report.py so the tables and the write-up cannot drift.

Two grades exist and they answer different questions:

  * `grade_auto` (in the detail file) is the pipeline's own coarse verdict: an
    answer with a parseable citation is "correct".
  * `quality_manual.csv` is the hand-verified fact grade: for each question I
    read the answer against the cited page. `results.csv`, Table 3, and the
    report use this one, because "Correct and cited" means the fact is right,
    not just that a citation parsed.

`real_summary()` refuses to return numbers until every question has a manual
verdict, so an ungraded sweep can never be published as an accuracy figure.
"""
import csv
import json
import os

SETTING = "p220_k6_ryes"
QUESTIONS = 20
TPM = 8000.0

# Table 3 needs answer quality for at least three settings. One 20-question
# real sweep is the §3 measurement; the two comparison settings use the first
# ten questions (the same subset) because every extra setting costs a full day
# of token refill on a 200K-token day. A detail file that does not exist yet is
# skipped, so the table can be built while the later sweeps are still running.
REAL_RUNS = [
    ("p220_k6_ryes", "quality_real.detail.jsonl", "quality_manual.csv", 20),
    ("p120_k4_rno", "quality_p120.detail.jsonl", "quality_manual_p120.csv", 10),
    ("p450_k3_ryes", "quality_p450.detail.jsonl", "quality_manual_p450.csv", 10),
]

VERDICTS = ("correct", "wrong", "refused_correctly", "refused_wrongly", "not_measured")
MANUAL_COLS = ("qid", "verdict", "page_ok", "note")


def read_jsonl(path):
    rows = []
    for line in open(path, encoding="utf-8"):
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def read_csv(path):
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def pctl(values, p):
    """Nearest-rank percentile: item ceil(p*n/100), 1-based, of the sorted values.

    Same rule as scripts/analyze.py and cmd/relay, so a table cell, the raw
    file, and the harness summary cannot disagree.
    """
    vs = sorted(v for v in values if v is not None)
    if not vs:
        return float("nan")
    k = int(-(-p * len(vs) // 100))       # ceil, integer arithmetic
    return vs[min(max(k, 1), len(vs)) - 1]


def load_manual(path):
    if not os.path.exists(path):
        return None
    rows = read_csv(path)
    if not rows:
        return None
    out = {}
    for r in rows:
        qid = (r.get("qid") or "").strip()
        if not qid:
            continue
        verdict = (r.get("verdict") or "").strip()
        if verdict not in VERDICTS:
            raise SystemExit("%s: %s has verdict %r; allowed: %s"
                             % (path, qid, verdict, ", ".join(VERDICTS)))
        out[qid] = r
    return out


def real_summary(detail_path, manual_path, setting=SETTING, expected=QUESTIONS):
    rows = [d for d in read_jsonl(detail_path) if d.get("setting") == setting]
    if not rows:
        raise SystemExit("no %s rows in %s yet" % (setting, detail_path))

    manual = load_manual(manual_path)
    qids = [d["qid"] for d in rows]
    if manual is None:
        raise SystemExit(
            "missing %s: the Table 3 quality columns are hand-verified facts, "
            "not the citation-only automatic grade. Grade the %d answers first:\n"
            "  python scripts/review_quality.py\n"
            "then write one row per question (%s)."
            % (manual_path, len(rows), ",".join(MANUAL_COLS)))
    missing = [q for q in qids if q not in manual]
    extra = [q for q in manual if q not in qids]
    if missing or extra:
        raise SystemExit("%s does not match the sweep: missing %s, extra %s"
                         % (manual_path, missing or "none", extra or "none"))
    if len(rows) != expected:
        raise SystemExit("sweep incomplete: %d/%d rows for %s"
                         % (len(rows), expected, setting))

    counted = {k: 0 for k in VERDICTS}
    graded = []
    for d in rows:
        v = manual[d["qid"]]["verdict"]
        counted[v] += 1
        # A question the sweep could not run has no answer to grade.
        if d.get("grade_auto") == "not_measured" and v != "not_measured":
            raise SystemExit("%s was budget-stopped but graded %r" % (d["qid"], v))
        if v != "not_measured":
            graded.append(d)

    evaluated = len(graded)
    if not evaluated:
        raise SystemExit("every question was budget-stopped; nothing to report")
    tokens = sum(int(d.get("tokens_in", 0)) + int(d.get("tokens_out", 0)) for d in rows)
    ttft = [float(d.get("ttft_ms", 0.0)) for d in graded]
    total = [float(d.get("total_ms", 0.0)) for d in graded]
    # Two kinds of waiting are reported: the time the harness deliberately
    # paused to earn the next request's tokens (pacing_ms), and any wait the
    # pipeline itself recorded (waiting_ms). Section 4 quotes their sum.
    waits = [float(d.get("pacing_ms", 0.0)) + float(d.get("waiting_ms", 0.0))
             for d in rows]
    waits = [w for w in waits if w > 0]

    def pct(n):
        return 100.0 * n / evaluated

    return {
        "asked": len(rows),
        "evaluated": evaluated,
        "not_measured": len(rows) - evaluated,
        "correct": counted["correct"],
        "wrong": counted["wrong"],
        "refused_correctly": counted["refused_correctly"],
        "refused_wrongly": counted["refused_wrongly"],
        "correct_pct": pct(counted["correct"]),
        "wrong_pct": pct(counted["wrong"]),
        "refused_correct_pct": pct(counted["refused_correctly"]),
        "refused_wrong_pct": pct(counted["refused_wrongly"]),
        "tokens_per_answer": tokens / evaluated,
        "ttft_p50": pctl(ttft, 50), "ttft_p95": pctl(ttft, 95),
        "total_p50": pctl(total, 50), "total_p95": pctl(total, 95),
        "wait_min": min(waits) / 60000.0 if waits else float("nan"),
        "wait_max": max(waits) / 60000.0 if waits else float("nan"),
        "retries": sum(int(d.get("retries", 0)) for d in rows),
        "page_ok": sum(1 for d in rows
                       if manual[d["qid"]].get("page_ok", "").strip() == "yes"),
    }


def table3_rows(res_dir):
    """Every Table 3 row, real sweeps first, then stand-in cost/latency rows.

    A real run is included only when its detail file exists *and* every answer
    has a hand verdict; real_summary enforces both. Stand-in rows carry tokens
    and latency but no quality, because the stand-in's draft is canned text.
    """
    rows, real_settings = [], set()
    for setting, det_name, man_name, expected in REAL_RUNS:
        det = os.path.join(res_dir, det_name)
        if not os.path.exists(det):
            continue
        s = real_summary(det, os.path.join(res_dir, man_name), setting, expected)
        real_settings.add(setting)
        rows.append(dict(s, setting=setting, source="real"))

    stand_path = os.path.join(res_dir, "retrieval_standin.csv")
    if os.path.exists(stand_path):
        for r in read_csv(stand_path):
            if r["setting"] in real_settings:
                continue
            rows.append({
                "setting": r["setting"], "source": "stand-in",
                "asked": int(r["questions"]), "evaluated": int(r["questions"]),
                "correct_pct": None, "wrong_pct": None,
                "refused_correct_pct": None,
                "tokens_per_answer": float(r["tokens_per_answer"]),
                "ttft_p50": float(r["ttft_p50"]),
                "ttft_p95": float(r["ttft_p95"]),
                "total_p50": float(r["total_p50"]),
                "total_p95": float(r["total_p95"]),
            })
    if len(rows) < 3:
        raise SystemExit("Table 3 needs at least three settings, built %d" % len(rows))
    return rows
