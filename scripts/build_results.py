"""Rebuild the authoritative results/results.csv from two measured sources:

  1. the stand-in load rows in results/load_results.csv (one row per request
     of each of the five load shapes), and
  2. the real-service quality sweeps at three retrieval settings, whose raw
     per-question records are results/quality_*.detail.jsonl.

The quality rows carry the **hand-verified** fact verdict once
results/quality_manual*.csv exists, so a reviewer can recompute Table 3 from
this file alone; the automatic citation-only verdict stays in the detail file.

Three value translations are deliberate, and each is stated in the report:

  * `busy` -> `timeout`. The brief allows exactly four status values. A `busy`
    row is our admission control giving up before a provider call; it is not a
    mid-flight HTTP timeout, and the prose says so.
  * daily-budget stoppage -> `rate_limited` (the provider's own 429).
  * a stand-in row's answer_grade is "not measured": the stand-in returns
    canned text, so grading it would invent a quality number.

Inputs are read-only; the output is written atomically.
"""
import argparse
import csv, json, os, sys

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
R = os.path.join(BASE, "results")
CSV = os.path.join(R, "results.csv")
LOAD = os.path.join(R, "load_results.csv")

REAL_RUNS = [
    # setting, detail, hand grades, scenario label, request-id prefix
    ("p220_k6_ryes", "quality_real.detail.jsonl", "quality_manual.csv",
     "quality_sweep", "real"),
    ("p120_k4_rno", "quality_p120.detail.jsonl", "quality_manual_p120.csv",
     "quality_sweep", "real"),
    ("p450_k3_ryes", "quality_p450.detail.jsonl", "quality_manual_p450.csv",
     "quality_sweep", "real"),
    # The repair run is a pipeline variant on the same setting and questions,
    # reported separately and never merged into the baseline: its scenario is
    # quality_fix20 and its request ids start with "fix".
    ("p220_k6_ryes", "quality_fix20.detail.jsonl", "quality_manual_fix20.csv",
     "quality_fix20", "fix"),
]

COLS = ["request_id", "scenario", "source", "start_time", "first_token_ms",
        "total_ms", "status", "retries", "tokens_in", "tokens_out",
        "retrieval_setting", "answer_grade"]

LOAD_ORDER = ["sustainable", "steady", "burst", "overload", "rpm_half", "daily_exhausted"]


def grade_label(g):
    return "not measured" if g in ("not_measured", "not measured") else g


def manual_verdicts(path):
    if not os.path.exists(path):
        return {}
    with open(path, newline="", encoding="utf-8") as f:
        return {(r.get("qid") or "").strip(): (r.get("verdict") or "").strip()
                for r in csv.DictReader(f) if (r.get("qid") or "").strip()}


def main():
    global R, CSV, LOAD
    ap = argparse.ArgumentParser()
    ap.add_argument("--results", default=R,
                    help="results directory (default: results/)")
    a = ap.parse_args()
    R = a.results
    CSV = os.path.join(R, "results.csv")
    LOAD = os.path.join(R, "load_results.csv")
    runs = [(s, os.path.join(R, d), os.path.join(R, m), sc, pre)
            for s, d, m, sc, pre in REAL_RUNS]
    present = [(s, d, m, sc, pre) for s, d, m, sc, pre in runs
               if os.path.exists(d)]
    if not present:
        sys.exit("no real sweep detail found (expected %s)" % runs[0][1])
    if not os.path.exists(LOAD):
        sys.exit("missing %s (run scripts/run_load.ps1 first)" % LOAD)

    # ---- measured stand-in load rows, relabelled to the brief term
    with open(LOAD, newline="", encoding="utf-8") as f:
        load = list(csv.DictReader(f))
    for r in load:
        if r["source"] in ("standin", "stand-in"):
            r["source"] = "stand-in"
        # The brief has no `busy` status: a shed request is a `timeout` in its
        # vocabulary (the person gave up waiting), and the prose distinguishes
        # it from a mid-flight HTTP timeout.
        if r["status"] == "busy":
            r["status"] = "timeout"
        # Canned stand-in text is not an answer, so it has no quality verdict.
        r["answer_grade"] = "not measured"
    missing = [s for s in LOAD_ORDER if s not in {r["scenario"] for r in load}]
    if missing:
        sys.exit("load_results.csv is missing scenarios: %s" % ", ".join(missing))

    # ---- real-service quality rows, one block per setting
    real = []
    for setting, detail_path, manual_path, scenario, prefix in present:
        with open(detail_path, encoding="utf-8") as f:
            detail = [json.loads(l) for l in f if l.strip()]
        manual = manual_verdicts(manual_path)
        if not manual:
            print("WARNING: %s missing; %s rows keep the automatic citation "
                  "grade" % (manual_path, setting), file=sys.stderr)
        for d in detail:
            stopped = bool(d.get("day_exhausted")) and int(d.get("tokens_in", 0)) == 0
            status = d.get("status") or "ok"
            if status == "busy":
                status = "timeout"
            if stopped:
                status = "rate_limited"
            grade = manual.get(d["qid"]) or d.get("grade_auto", "")
            real.append({
                "request_id": "%s-%s-%s" % (prefix, setting, d["qid"]),
                "scenario": scenario,
                "source": "real",
                "start_time": d.get("start_time") or "not measured",
                "first_token_ms": "%.1f" % float(d.get("ttft_ms", 0.0)),
                "total_ms": "%.1f" % float(d.get("total_ms", 0.0)),
                "status": status,
                "retries": str(int(d.get("retries", 0))),
                "tokens_in": str(int(d.get("tokens_in", 0))),
                "tokens_out": str(int(d.get("tokens_out", 0))),
                "retrieval_setting": d.get("setting") or setting,
                "answer_grade": grade_label(grade),
            })

    # ---- stand-in retrieval sweep rows (Table 3's cost/latency comparison rows)
    standin_ret = []
    standin_det = os.path.join(R, "retrieval_standin.detail.jsonl")
    if os.path.exists(standin_det):
        with open(standin_det, encoding="utf-8") as f:
            for line in f:
                if not line.strip():
                    continue
                d = json.loads(line)
                st = d.get("status") or "ok"
                if st == "busy":
                    st = "timeout"
                setting = d.get("setting") or ""
                standin_ret.append({
                    "request_id": "standin-%s-%s" % (setting, d["qid"]),
                    "scenario": "retrieval_standin",
                    "source": "stand-in",
                    "start_time": d.get("start_time") or "not measured",
                    "first_token_ms": "%.1f" % float(d.get("ttft_ms", 0.0)),
                    "total_ms": "%.1f" % float(d.get("total_ms", 0.0)),
                    "status": st,
                    "retries": str(int(d.get("retries", 0))),
                    "tokens_in": str(int(d.get("tokens_in", 0))),
                    "tokens_out": str(int(d.get("tokens_out", 0))),
                    "retrieval_setting": setting,
                    "answer_grade": "not measured",
                })

    load.sort(key=lambda r: (LOAD_ORDER.index(r["scenario"])
                             if r["scenario"] in LOAD_ORDER else 99,
                             int(r["request_id"].split("-")[-1])
                             if r["request_id"].split("-")[-1].isdigit() else 0))
    rows = real + standin_ret + load

    tmp = CSV + ".tmp"
    with open(tmp, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=COLS)
        w.writeheader()
        w.writerows(rows)
    os.replace(tmp, CSV)

    nm = sum(1 for r in real if r["answer_grade"] == "not measured")
    print("wrote", CSV)
    print("total rows:", len(rows), "| real:", len(real),
          "(not measured: %d)" % nm, "| retrieval_standin:", len(standin_ret),
          "| load:", len(load))


if __name__ == "__main__":
    main()
