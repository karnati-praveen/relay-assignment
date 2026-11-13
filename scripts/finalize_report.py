"""Patch report.md's sweep-dependent numbers from the measured real detail.

The write-up must not drift from `results/tables.md`. Every figure that depends
on the real 20-question sweep comes from scripts/sweep_stats.py, the same module
analyze.py uses, so the two cannot disagree. Each replacement is applied by
*pattern* with an assertion that it matches exactly once; if the sweep is
incomplete or a sentence has been hand-edited into a shape this script does not
recognise, it refuses and changes nothing.

Also regenerates `results/quality_manual.md` from `quality_manual.csv`, so the
prose record of the hand grading is never stale.

Patch source: `report.template.md` when it exists (the version with the TBD_*
placeholders), otherwise `report.md`. The placeholders are consumed by the first
run, so patching the already-patched file would fail on the first TBD it no
longer contains. Edit the template when the prose changes; `report.md` is an
output.

Usage:
  python scripts/finalize_report.py            # patch report.md
  python scripts/finalize_report.py --check    # report diffs, write nothing
"""
import argparse
import csv
import json
import os
import re
import statistics
import sys

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
R = os.path.join(BASE, "results")
RES = R                      # overridable for a dry run
sys.path.insert(0, os.path.join(BASE, "scripts"))
import sweep_stats as SS
import analyze as AN

SETTING = SS.SETTING
TPM = SS.TPM
PRICE_IN, PRICE_OUT = 0.075, 0.30


def read_csv(path):
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def table3_rows(s):
    """Rebuild every data row of Table 3, from the same numbers as tables.md."""
    out = []
    for r in SS.table3_rows(RES):
        qcell = (str(r["asked"]) if r["asked"] == r["evaluated"]
                 else "%d (%d graded)" % (r["asked"], r["evaluated"]))
        if r["source"] == "real":
            out.append("| %s **(real)** | %s | %.1f | %.1f | %.1f | %.0f | %.1f / %.1f | %.1f / %.1f |" % (
                r["setting"], qcell, r["correct_pct"], r["wrong_pct"],
                r["refused_correct_pct"], r["tokens_per_answer"],
                r["ttft_p50"], r["ttft_p95"], r["total_p50"], r["total_p95"]))
        else:
            out.append("| %s (stand-in) | %s | not measured | not measured | not measured | %.0f | %.1f / %.1f | %.1f / %.1f |" % (
                r["setting"], qcell, r["tokens_per_answer"],
                r["ttft_p50"], r["ttft_p95"], r["total_p50"], r["total_p95"]))
    return out


def replace_table(text, header, rows):
    """Replace a markdown table's data rows, leaving its separator alone."""
    start = text.index(header)
    lines = text[start:].split("\n")
    i = 2
    while i < len(lines) and lines[i].startswith("|"):
        i += 1
    new = [header, lines[1]] + list(rows)
    return text[:start] + "\n".join(new) + "\n" + "\n".join(lines[i:])


def load_detail(name):
    path = os.path.join(RES, name)
    if not os.path.exists(path):
        sys.exit("missing %s" % path)
    return [json.loads(l) for l in open(path, encoding="utf-8") if l.strip()]


def one(pat, text, repl, label):
    """Replace exactly one match, or refuse. Keeps the report honest.

    Uses re.sub so the replacement can carry backreferences; a plain string
    splice would insert the literal '\\g<1>' text.
    """
    hits = list(re.finditer(pat, text, flags=re.S))
    if len(hits) != 1:
        sys.exit("pattern %r matched %d times (want 1): %s" % (pat, len(hits), label))
    return re.sub(pat, repl, text, count=1, flags=re.S)


def write_manual(path, detail_path, manual_path):
    """Render results/quality_manual.md from the hand-verdict CSV."""
    detail = {d["qid"]: d for d in SS.read_jsonl(detail_path)
              if d.get("setting") == SETTING}
    manual = SS.load_manual(manual_path)
    if manual is None:
        return False
    graded = [d for d in detail.values()
              if manual[d["qid"]]["verdict"] != "not_measured"]
    diff = [(d["qid"], d.get("grade_auto"), manual[d["qid"]]["verdict"])
            for d in detail.values()
            if manual[d["qid"]]["verdict"] != "not_measured"
            and d.get("grade_auto") != manual[d["qid"]]["verdict"]]
    lines = [
        "# Hand-verified quality grading",
        "",
        "The pipeline's automatic `grade_auto` (in `results/quality_real.detail.jsonl`)",
        "only checks that an answer carried a parseable citation. This file records",
        "whether the **fact** is actually right, read against the page the answer",
        "cited, from the pinned text in `papers/text/`. `results/results.csv` carries",
        "this hand-verified `answer_grade`, which is the grade Table 3 and the report quote.",
        "",
        "Ground truth: my own reading of Attention 1706.03762v7, Moshi",
        "2410.00037v1 and Dynamic Chunking 2507.07955v2. Page numbers are the",
        "`<<<PAGE n>>>` markers in the extracted text. Raw answers:",
        "`results/quality_real.detail.jsonl`.",
        "",
        "`page_ok` is separate from the fact: a right fact on a worse page still",
        "counts as correct, but the page is reported. `correct` means the fact is",
        "right *and* the answer cites a page where that fact appears; `wrong` means",
        "the fact is wrong, or no citation carries it.",
        "",
        "| Q | Question | Verdict | Cited page | Page best? | Note |",
        "|---|---|---|---|---|---|",
    ]
    for q in sorted(detail):
        d, m = detail[q], manual[q]
        pages = ", ".join("%s p.%d" % (p, n) for p, n in
                          zip(d.get("citations") or [], d.get("cited_pages") or []))
        note = (m.get("note") or "").replace("|", "/").strip()
        lines.append("| %s | %s | %s | %s | %s | %s |" % (
            q, (d.get("question") or "").replace("|", "/"), m["verdict"],
            pages or "—", m.get("page_ok") or "—", note or "—"))
    lines += [
        "",
        "## Reconciliation with the automatic grade",
        "",
        "Automatic and hand grades disagree on **%d of %d** graded rows%s." % (
            len(diff), len(graded),
            (": " + ", ".join("%s auto=%s manual=%s" % t for t in diff)) if diff else ""),
        "",
    ]
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(lines))
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--detail", default=os.path.join(R, "quality_real.detail.jsonl"))
    ap.add_argument("--manual", default=os.path.join(R, "quality_manual.csv"))
    ap.add_argument("--report", default=os.path.join(BASE, "report.md"))
    ap.add_argument("--template", default=os.path.join(BASE, "report.template.md"),
                    help="unpatched source holding the TBD_* placeholders")
    ap.add_argument("--results", default=R, help="results dir (dry-run override)")
    ap.add_argument("--check", action="store_true")
    a = ap.parse_args()
    global RES
    RES = a.results

    s = SS.real_summary(a.detail, a.manual)
    if s["tokens_per_answer"] > 6286:
        # The whole-request reservation in section 5 is 6,286 tokens (passages
        # + one draft + one check + headroom). A mean above the reservation is a
        # finding, not a reason to stop: the reservation covers the expected
        # path, not a revision. Flag it so section 5's wording is checked by eye
        # before the PDF is rendered.
        print("WARNING: measured %.0f tokens/answer exceeds the 6,286-token "
              "reservation; section 5 must say the reservation covers one draft "
              "plus one check, not a revision" % s["tokens_per_answer"],
              file=sys.stderr)

    stand_tokens = {r["setting"]: float(r["tokens_per_answer"])
                    for r in read_csv(os.path.join(RES, "retrieval_standin.csv"))}
    standin_mean = stand_tokens[SETTING]

    tok = s["tokens_per_answer"]
    tok_comma = "{:,}".format(round(tok))
    pct_real = round(tok / TPM * 100.0)          # share of one minute's tokens
    ans_real = round(TPM / tok, 1)               # answers one minute's tokens buy
    delta = round(abs(standin_mean - tok) / tok * 100.0)
    # The cheap setting is measured, not the stand-in's character count, so its
    # answers/minute uses the real row's tokens per answer.
    cheap_row = {r["setting"]: r for r in SS.table3_rows(RES)
                 if r["source"] == "real"}.get("p120_k4_rno")
    if cheap_row is None:
        sys.exit("Table 3 has no real p120_k4_rno row yet")
    cheap_ans = round(TPM / cheap_row["tokens_per_answer"], 1)
    factor = round(6386.0 / tok, 1)              # 1's "2.2x the answers/minute"
    wait_lo, wait_hi = int(round(s["wait_min"])), int(round(s["wait_max"]))
    wait_txt = "%d" % wait_lo if wait_lo == wait_hi else "%d–%d" % (wait_lo, wait_hi)

    # Patch from the pristine template, never from the last patched report: the
    # placeholders are consumed by the first run, so a retry that started from
    # report.md would fail at the first TBD it no longer has. Keeping the
    # template also means a failed release run can be re-run unchanged.
    src = a.template if os.path.exists(a.template) else a.report
    text = open(src, encoding="utf-8").read()
    orig = text

    # --- Table 3: regenerate every data row -------------------------------
    start = text.index("| Setting (piece size, pieces fetched, re-rank yes or no)")
    end = text.index("\n\n", start)
    head = text[start:end].split("\n")[:2]          # header + separator
    text = text[:start] + "\n".join(head + table3_rows(s)) + text[end:]

    # --- Tables 2a/2b: regenerate from the load rows ----------------------
    meta = json.load(open(os.path.join(RES, "scenario_meta.json"), encoding="utf-8"))
    allrows = read_csv(os.path.join(RES, "results.csv"))
    loadrows = [r for r in allrows if r["scenario"] in AN.SCENARIO_LABEL]
    text = replace_table(text, AN.TABLE_2A_HEADER,
                         AN.table_2a_rows(loadrows, meta))
    text = replace_table(text, AN.TABLE_2B_HEADER,
                         AN.table_2b_rows(loadrows, meta))

    by = {}
    for r in loadrows:
        by.setdefault(r["scenario"], []).append(r)
    steady = by["steady"]
    steady_pct = 100.0 * sum(1 for r in steady if r["status"] == "ok") / len(steady)
    giveups = [r for r in loadrows if r["status"] == "timeout"]
    daily = [r for r in loadrows if r["status"] == "rate_limited"]
    errors = [r for r in loadrows if r["status"] == "error"]
    ref_p50 = SS.pctl([float(r["total_ms"]) for r in giveups], 50)
    ref_p95 = SS.pctl([float(r["total_ms"]) for r in giveups], 95)
    n_ref = len(giveups) + len(daily) + len(errors)
    tok2b, cost2b = [], []
    for sc, rs in by.items():
        ok = sum(1 for r in rs if r["status"] == "ok")
        tin = sum(int(r["tokens_in"] or 0) for r in rs)
        tout = sum(int(r["tokens_out"] or 0) for r in rs)
        tok2b.append((tin + tout) / max(1, ok))
        cost2b.append(((tin * PRICE_IN + tout * PRICE_OUT) / 1e6)
                      / max(1, ok) * 1000)
    tok_lo, tok_hi = min(tok2b), max(tok2b)
    cost_mean = sum(cost2b) / len(cost2b)
    total_load = len(loadrows)

    # --- section 3's inline numbers, all from the same load rows ----------
    text = one(r"(only \*\*)[\d.]+(%\*\* were\s+answered)", text,
               r"\g<1>%.1f\g<2>" % steady_pct, "S3 steady answered")
    text = one(r"(refused locally in \*\*)[\d.]+( ms\*\* for \*\*zero tokens\*\*)",
               text, r"\g<1>%.1f\g<2>" % ref_p50, "S3 refusal median")
    text = one(r"(flat \()[\d,]+–[\d,]+(\) and 1,000 answers cost ≈\*\*\$)[\d.]+",
               text, r"\g<1>{:,.0f}–{:,.0f}\g<2>{:.2f}".format(tok_lo, tok_hi, cost_mean),
               "S3 tokens/cost flatness")
    text = one(r"(timeouts\*\* in )[\d,]+( requests)", text,
               r"\g<1>{:,}\g<2>".format(total_load), "S3 request count")
    text = one(r"(\*\*)[\d,]+( of )[\d,]+(\*\* were given up)", text,
               r"\g<1>{:,}\g<2>{:,}\g<3>".format(n_ref, total_load),
               "S3 give-up count")
    text = one(r"(\*\*)[\d,]+(\*\* by admission control and \*\*)[\d,]+(\*\* by the daily budget)",
               text, r"\g<1>{:,}\g<2>{:,}\g<3>".format(len(giveups), len(daily)),
               "S3 give-up split")

    # --- section 5: the refusals, and the reservation it protects ---------
    text = one(r"(pointers in\s+\*\*)[\d.]+(\s+ms p50 / )[\d.]+(\s+ms p95\*\*)", text,
               r"\g<1>%.1f\g<2>%.1f\g<3>" % (ref_p50, ref_p95), "S5 refusal p95")
    text = one(r"(\*\*)[\d,]+(\*\* of )[\d,]+(\s+requests,\s+returning\s+local pointers)",
               text,
               r"\g<1>{:,}\g<2>{:,}\g<3>".format(len(giveups), total_load),
               "S5 admission count")
    text = one(r"(\*\*)[\d,]+(\*\*\s+more\s+were\s+dropped by the\s+daily budget,\s+\*\*)[\d,]+(\s+of\s+)[\d,]+(\s+in all\*\*)",
               text,
               r"\g<1>{:,}\g<2>{:,}\g<3>{:,}\g<4>".format(
                   len(daily), n_ref, total_load),
               "S5 daily count")

    # --- section 4: the real sweep's median, and one request's split ------
    text = one(r"TBD_P50", text, "{:,.1f}".format(s["total_p50"]), "S4 median total")
    text = one(r"TBD_TTFT", text, "{:,.1f}".format(s["ttft_p50"]), "S4 median ttft")

    ld = load_detail("load_detail.jsonl")
    pool = [d for d in ld if d["scenario"] == "steady"
            and d["status"] == "ok" and d["first_token_ms"] > 0]
    if not pool:
        sys.exit("no answered steady request in load_detail.jsonl to split")
    pick = max(pool, key=lambda d: d["total_ms"])
    tot, wait = float(pick["total_ms"]), float(pick["waiting_ms"])
    retry, ttft = float(pick["retrying_ms"]), float(pick["first_token_ms"])
    work = float(pick.get("working_ms", 0.0))
    # The three buckets are disjoint by construction. If they do not add up to
    # the measured total (rounding, or a step that fell outside the trace),
    # fall back to the residual so the published shares always sum to 100%.
    if abs(wait + retry + work - tot) > max(50.0, 0.02 * tot):
        print("warning: %s split %.1f+%.1f+%.1f != %.1f; using residual"
              % (pick["request_id"], wait, retry, work, tot), file=sys.stderr)
        work = max(0.0, tot - wait - retry)
    text = one(r"TBD_REQ_TTFTPCT", text, "%.1f" % (100 * ttft / tot), "S4 before-token share")
    text = one(r"TBD_REQ_WAITPCT", text, "%.1f" % (100 * wait / tot), "S4 waiting share")
    text = one(r"TBD_REQ_RETRYPCT", text, "%.1f" % (100 * retry / tot), "S4 retrying share")
    text = one(r"TBD_REQ_WORKPCT", text, "%.1f" % (100 * work / tot), "S4 working share")
    text = one(r"TBD_REQ_TOTAL", text, "{:,.1f}".format(tot), "S4 request total")
    text = one(r"TBD_REQ_WAIT", text, "{:,.1f}".format(wait), "S4 request waiting")
    text = one(r"TBD_REQ_RETRY", text, "{:,.1f}".format(retry), "S4 request retrying")
    text = one(r"TBD_REQ_WORK", text, "{:,.1f}".format(work), "S4 request working")
    text = one(r"TBD_REQ_TTFT", text, "{:,.1f}".format(ttft), "S4 request ttft")
    text = one(r"TBD_REQ", text, pick["request_id"], "S4 request id")
    # The split of the working time at the first token: everything before the
    # token that was not waiting or retry backoff is plan + retrieval + prefill.
    pre = max(0.0, ttft - wait - retry)
    post = max(0.0, work - pre)
    text = one(r"(— so )\d+( ms of\s+the working time was plan, retrieval and "
               r"prefill, and )\d+( ms decode and check)", text,
               r"\g<1>%.0f\g<2>%.0f\g<3>" % (pre, post), "S4 pre/post token split")

    # --- retrieval: quality at the three real settings ---------------------
    t3 = SS.table3_rows(RES)
    def row(setting):
        for r in t3:
            if r["setting"] == setting and r["source"] == "real":
                return r
        sys.exit("Table 3 has no real row for %s yet" % setting)
    p120 = row("p120_k4_rno")
    text = one(r"TBD_CORRECT", text, "%.1f" % s["correct_pct"], "T3 chosen correct")
    text = one(r"TBD_WRONG", text, "%.1f" % s["wrong_pct"], "T3 chosen wrong")
    # The brief's third quality column is "Correctly refused %", so a row adds up
    # to 100% only when the omitted refused_wrongly share is stated. It is said
    # twice (Table 3's prose and the assistant section), so replace every copy;
    # a missing placeholder is still a failure.
    if "TBD_REFW" not in text:
        sys.exit("missing TBD_REFW")
    text = text.replace("TBD_REFW", "%.1f" % s["refused_wrong_pct"])
    text = one(r"TBD_P120_CORRECT", text, "%.1f" % p120["correct_pct"], "T3 p120 correct")
    text = one(r"TBD_P120_TOK", text, "{:,.0f}".format(p120["tokens_per_answer"]),
               "T3 p120 tokens")

    # --- the repair run: same 20 questions, counted against the same 17 -----
    # The denominator is the answerable questions only: a repair that turns a
    # refusal into a right answer has not "fixed" a trap question, and the
    # three trap questions must still be refused. Counting over all 20 would
    # let a lenient checker look better by answering a question it should not.
    qdata = json.load(open(os.path.join(BASE, "data", "questions.json"),
                           encoding="utf-8"))
    answerable = [q["id"] for q in qdata["questions"]
                  if q["expect"] in ("correct", "adversarial")]
    if len(answerable) != 17:
        sys.exit("questions.json has %d answerable questions, expected 17"
                 % len(answerable))
    allq = read_csv(os.path.join(RES, "results.csv"))
    base_grades = {r["request_id"].split("-")[-1]: r["answer_grade"] for r in allq
                   if r["scenario"] == "quality_sweep"
                   and r["retrieval_setting"] == SETTING}
    fix_grades = {r["request_id"].split("-")[-1]: r["answer_grade"] for r in allq
                  if r["scenario"] == "quality_fix20"}
    if len(base_grades) != 20 or len(fix_grades) != 20:
        sys.exit("repair comparison needs 20 baseline and 20 fix rows, got %d/%d"
                 % (len(base_grades), len(fix_grades)))

    def tally(grades):
        def count(v):
            return sum(1 for q in answerable if grades[q] == v)
        return count("correct"), count("wrong"), count("refused_wrongly")

    b_correct, b_wrong, b_rw = tally(base_grades)
    f_correct, f_wrong, f_rw = tally(fix_grades)
    print("repair: baseline %d/17 correct, %d wrong, %d refused_wrongly | "
          "fixed %d/17, %d wrong, %d refused_wrongly"
          % (b_correct, b_wrong, b_rw, f_correct, f_wrong, f_rw))
    text = one(r"TBD_BASE_CORRECT", text, "%d" % b_correct, "repair baseline correct")
    text = one(r"TBD_BASE_ANS_PCT", text, "%.1f" % (100.0 * b_correct / 17),
               "repair baseline pct")
    text = one(r"TBD_FIX_CORRECT", text, "%d" % f_correct, "repair fixed correct")
    text = one(r"TBD_FIX_ANS_PCT", text, "%.1f" % (100.0 * f_correct / 17),
               "repair fixed pct")
    text = one(r"TBD_BASE_RW", text, "%d" % b_rw, "repair baseline refusals")
    text = one(r"TBD_FIX_RW", text, "%d" % f_rw, "repair fixed refusals")
    text = one(r"TBD_BASE_WRONG", text, "%d" % b_wrong, "repair baseline wrong")
    text = one(r"TBD_FIX_WRONG", text, "%d" % f_wrong, "repair fixed wrong")

    # --- the single tokens-per-answer figure, in every place it is cited ---
    # Section 1 states measured cost, the ratio to the original 6,386-token
    # design, and *why* the ratio is smaller than the 2.2x first reported: the
    # checker now reads whole passages. Those three must move together.
    text = one(r"TBD_REAL_TOK", text, tok_comma, "S1 measured tokens/answer")
    text = one(r"(one answer costs\s+\*\*)[\d,]+( tokens\*\*\s+on\s+the\s+real\s+service)", text,
               r"\g<1>" + tok_comma + r"\g<2>", "S3 answer cost")
    text = one(r"(\*\*)[\d.]+(% of a minute's budget\*\*)", text,
               r"\g<1>%d\g<2>" % pct_real, "S3 percent of minute")
    text = one(r"(buys\s+\*\*)[\d.]+(\s+answers, not 30\*\*)", text,
               r"\g<1>%.1f\g<2>" % ans_real, "S3 answers/minute")
    # The stand-in's character count is the cross-check on the real figure; both
    # the count and the gap are computed so the claim stays true if either moves.
    text = one(r"(are the stand-in's character count\s*\()[\d,]*\d"
               r"(\s*,\s+within\s+)[\d.]+(% of the real\s+)[\d,]*\d", text,
               r"\g<1>{:,}\g<2>{}\g<3>{:,}".format(round(standin_mean), delta, round(tok)),
               "S3 stand-in cross-check")
    text = one(r"(all 20 questions:\s*\*\*)[\d,]+(\s+tokens/answer\*\*)", text,
               r"\g<1>" + tok_comma + r"\g<2>", "retrieval chosen cost")
    text = one(r"TBD_CHEAP_ANS", text, "%.1f" % cheap_ans, "S5 cheap ceiling")
    text = one(r"(ceiling from )[\d.]+(\s+to\s+)[\d.]+", text,
               r"\g<1>%.1f\g<2>%.1f" % (ans_real, cheap_ans),
               "retrieval ceiling")
    text = one(r"(\*\*)\d+(?:–\d+)?(\s+minutes of pacing and waiting per question\*\*)",
               text, r"\g<1>" + wait_txt + r"\g<2>", "S4 wait per question")

    # --- section 6: the retry policy's measured count ----------------------
    text = one(r"(all six load shapes \()[\d,]+(\s+requests\))", text,
               r"\g<1>{:,}\g<2>".format(total_load), "S6 load request count")

    # --- section 7: the quota reply's latency is measured, so read it -------
    # results/daily_exhausted_reply.jsonl is five runs against an empty day
    # bucket; the median and the range are quoted in the prose.
    dre = [d for d in (json.loads(l) for l in
                       open(os.path.join(RES, "daily_exhausted_reply.jsonl"),
                            encoding="utf-8") if l.strip())]
    if not dre:
        sys.exit("daily_exhausted_reply.jsonl is empty; section 7 cannot quote a latency")
    ms = sorted(float(d["total_ms"]) for d in dre)
    text = one(r"(\*\*)[\d.]+( ms\*\* median)", text,
               r"\g<1>%.1f\g<2>" % statistics.median(ms), "S7 reply median")
    text = one(r"(\(n=)\d+(, )[\d.]+(–)[\d.]+(\))", text,
               r"\g<1>%d\g<2>%.1f\g<3>%.1f\g<4>" % (len(ms), ms[0], ms[-1]),
               "S7 reply range")

    left = sorted(set(re.findall(r"TBD_[A-Z0-9_]+", text)))
    if left:
        sys.exit("unconsumed placeholders: %s" % ", ".join(left))

    if "--check" in sys.argv:
        import difflib
        sys.stdout.writelines(difflib.unified_diff(
            orig.splitlines(True), text.splitlines(True), "report.md", "report.md.new"))
        print("check only: nothing written")
        return

    # Nothing is written until every patch above has matched. A failure halfway
    # used to leave report.md already rewritten, so a second run then failed on
    # the first TBD it no longer had — and the caller was told "untouched".

    # The supporting docs quote the same measured cost per answer. They are not
    # part of report.pdf, but they ship as evidence; a stale figure there would
    # contradict the write-up. Absent files (the dry-run fixture) are skipped.
    docs = [
        ("real_findings.md", r"(averaged \*\*)~?[\d,]+( tokens per answer\*\*)",
         r"\g<1>" + tok_comma + r"\g<2>", "findings tokens/answer"),
        ("real_findings.md", r"(i\.e\. \*\*)~?[\d.]+( answers per minute\*\*)",
         r"\g<1>" + "%.1f" % (TPM / tok) + r"\g<2>", "findings answers/minute"),
        ("real_findings.md", r"(most ~)\d+( answers per 200K-token day)",
         r"\g<1>" + str(int(200000 / tok)) + r"\g<2>", "findings answers/day"),
        ("measurement_plan.md", r"(one\s+request averages ~)[\d,]+(\s+tokens)",
         r"\g<1>" + tok_comma + r"\g<2>", "plan request cost"),
        ("measurement_plan.md", r"(one request costs ~)[\d,]+",
         r"\g<1>" + tok_comma, "plan request cost short"),
        ("conversation_history.md", r"(one answer costs\s+~?)[\d,]+( tokens)",
         r"\g<1>" + tok_comma + r"\g<2>", "history answer cost"),
    ]
    pending = []
    for rel, pat, repl, label in docs:
        path = os.path.join(RES, rel)
        if not os.path.exists(path):
            continue
        doc = open(path, encoding="utf-8").read()
        if rel == "conversation_history.md" and "## Appendix 5:" in doc:
            idx = doc.index("## Appendix 5:")
            new = one(pat, doc[:idx], repl, label) + doc[idx:]
        else:
            new = one(pat, doc, repl, label)
        if new != doc:
            pending.append((path, new))
    if text != orig:
        tmp = a.report + ".tmp"
        with open(tmp, "w", encoding="utf-8", newline="\n") as f:
            f.write(text)
        os.replace(tmp, a.report)
        print("patched", a.report)
    for path, new in pending:
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(new)
        print("patched", path)

    wrote_manual = write_manual(os.path.join(RES, "quality_manual.md"),
                                a.detail, a.manual)
    json.dump({"sweep": s, "standin_tokens": standin_mean,
               "cost_per_1000_usd": (tok * 0.85 * PRICE_IN
                                     + tok * 0.15 * PRICE_OUT) / 1e6 * 1000},
              open(os.path.join(RES, "report_numbers.json"), "w", encoding="utf-8"),
              indent=2)
    print("sweep: asked=%d graded=%d not_measured=%d correct=%d wrong=%d "
          "refused_correct=%d refused_wrong=%d"
          % (s["asked"], s["evaluated"], s["not_measured"], s["correct"],
             s["wrong"], s["refused_correctly"], s["refused_wrongly"]))
    print("tokens/answer %.1f | ttft p50 %.1f | total p50 %.1f | retries %d"
          % (tok, s["ttft_p50"], s["total_p50"], s["retries"]))
    if wrote_manual:
        print("wrote", os.path.join(RES, "quality_manual.md"))


if __name__ == "__main__":
    main()
