"""Print the real-service sweep as a review sheet for hand-grading.

The automatic grade only checks that an answer cites something; it cannot tell
a right fact from a wrong one. This script puts each question next to the text
of every page the answer cited, so the fact and the page can be checked by
reading, not by trusting the model.

`--csv` writes/refreshes results/quality_manual.csv with one row per measured
question, keeping verdicts already filled in, so grading can be done in
sittings. `--ungraded` then shows only what is left.

Grading rule: `correct` means the fact is right *and* the answer cites a page
where that fact appears; `wrong` means the fact is wrong, or no citation carries
it; `refused_correctly` / `refused_wrongly` judge only whether the refusal was
right, because a refusal makes no factual claim. `page_ok` is a separate, stricter
question — is the cited page the *best* page for the fact — and never changes the
verdict.

Usage:
  python scripts/review_quality.py                 # all questions
  python scripts/review_quality.py --csv           # refresh the verdict template
  python scripts/review_quality.py --ungraded      # only ungraded questions
"""
import argparse
import csv
import json
import os
import re

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DETAIL = os.path.join(BASE, "results", "quality_real.detail.jsonl")
MANUAL = os.path.join(BASE, "results", "quality_manual.csv")
TEXT = os.path.join(BASE, "papers", "text")
FILES = {
    "attention": "attention_1706.03762v7.txt",
    "moshi": "moshi_2410.00037v1.txt",
    "chunking": "chunking_2507.07955v2.txt",
}
PAGE_RE = re.compile(r"<<<PAGE (\d+)>>>")
VERDICTS = ("correct", "wrong", "refused_correctly", "refused_wrongly", "not_measured")
COLS = ("qid", "verdict", "page_ok", "note")


def pages(paper):
    path = os.path.join(TEXT, FILES[paper])
    out, cur, buf = {}, None, []
    for line in open(path, encoding="utf-8", errors="replace"):
        m = PAGE_RE.match(line)
        if m:
            if cur is not None:
                out[cur] = "".join(buf).strip()
            cur, buf = int(m.group(1)), []
        else:
            buf.append(line)
    if cur is not None:
        out[cur] = "".join(buf).strip()
    return out


def load_manual():
    if not os.path.exists(MANUAL):
        return {}
    with open(MANUAL, newline="", encoding="utf-8") as f:
        return {(r.get("qid") or "").strip(): r for r in csv.DictReader(f)}


def write_template(rows, existing):
    with open(MANUAL, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=COLS)
        w.writeheader()
        for d in sorted(rows, key=lambda r: r["qid"]):
            old = existing.get(d["qid"], {})
            w.writerow({"qid": d["qid"],
                        "verdict": old.get("verdict", ""),
                        "page_ok": old.get("page_ok", ""),
                        "note": old.get("note", "")})
    done = sum(1 for d in rows if existing.get(d["qid"], {}).get("verdict") in VERDICTS)
    print("wrote %s: %d rows, %d graded, %d to go"
          % (os.path.relpath(MANUAL, BASE), len(rows), done, len(rows) - done))
    print("verdicts: %s ...; page_ok: yes/no/n/a" % " | ".join(VERDICTS))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("detail", nargs="?", default=DETAIL)
    ap.add_argument("--csv", action="store_true", help="refresh the verdict template")
    ap.add_argument("--ungraded", action="store_true", help="only ungraded questions")
    ap.add_argument("--manual", default=None,
                    help="verdict CSV for this detail file (default: quality_manual.csv)")
    a = ap.parse_args()
    global MANUAL
    if a.manual:
        MANUAL = a.manual

    rows = [json.loads(l) for l in open(a.detail, encoding="utf-8") if l.strip()]
    qpath = os.path.join(BASE, "data", "questions.json")
    qs = {}
    if os.path.exists(qpath):
        qs = {q["id"]: q for q in json.load(open(qpath, encoding="utf-8"))["questions"]}
    existing = load_manual()
    if a.csv:
        write_template(rows, existing)

    cache = {}
    shown = 0
    for d in rows:
        verdict = existing.get(d["qid"], {}).get("verdict", "")
        if a.ungraded and verdict in VERDICTS:
            continue
        shown += 1
        q = qs.get(d["qid"], {})
        print("=" * 78)
        print("%s  expect=%s  auto=%s  status=%s  day_exhausted=%s  verdict=%s"
              % (d["qid"], d.get("expect"), d.get("grade_auto"),
                 d.get("status"), d.get("day_exhausted"), verdict or "-"))
        print("Q:", d.get("question") or q.get("text"))
        print("Why this expectation:", q.get("why", ""))
        print("Gold sentence pattern:", q.get("gold", []))
        print("A:", (d.get("answer") or "").strip())
        print("cites:", list(zip(d.get("citations") or [], d.get("cited_pages") or [])))
        print("-" * 78)
        for paper, page in zip(d.get("citations") or [], d.get("cited_pages") or []):
            if paper not in cache:
                cache[paper] = pages(paper)
            body = cache[paper].get(int(page), "(page not found)")
            print(">>> %s p.%s" % (paper, page))
            print(body[:1600])
            print("...")
        print()
    if a.ungraded:
        print("%d question(s) still ungraded." % shown)


if __name__ == "__main__":
    main()
