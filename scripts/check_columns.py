"""Self-check used at submission time: the raw file's columns and values must
match the brief exactly.

  1. the twelve required columns, in order, in internal/eval/eval.go;
  2. results/results.csv has exactly those twelve columns;
  3. every `status` and `answer_grade` is an allowed value (or the brief's
     "not measured").
"""
import csv, os, re, sys

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

required = ("request_id,scenario,source,start_time,first_token_ms,total_ms,"
            "status,retries,tokens_in,tokens_out,retrieval_setting,answer_grade").split(",")
# Exactly the brief's four values. The pipeline's internal `busy` (shed by our
# own admission control) is serialised as `timeout`, because the brief has no
# fifth value; the prose says these are give-ups, not mid-flight HTTP timeouts.
ALLOWED_STATUS = {"ok", "rate_limited", "timeout", "error"}
ALLOWED_GRADE = {"correct", "wrong", "refused_correctly", "refused_wrongly",
                 "not measured", "not_measured"}


def main():
    eval_go = os.path.join(BASE, "internal", "eval", "eval.go")
    src = open(eval_go, encoding="utf-8").read()
    m = re.search(r"var Columns = \[\]string\{(.*?)\}", src, re.S)
    ours = re.findall(r'"([^"]+)"', m.group(1))
    print("required n=%d: %s" % (len(required), required))
    print("ours     n=%d: %s" % (len(ours), ours))
    ok = required == ours
    print("COLUMN MATCH:", ok)
    for a, b in zip(required, ours):
        if a != b:
            print("  MISMATCH: required=%r ours=%r" % (a, b))

    csv_path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(BASE, "results", "results.csv")
    with open(csv_path, newline="", encoding="utf-8") as f:
        r = csv.DictReader(f)
        header = r.fieldnames
        rows = list(r)
    print("\nresults.csv columns:", header)
    print("RESULTS.CSV COLUMN MATCH:", header == required)
    bad = [row for row in rows
           if row["status"] not in ALLOWED_STATUS
           or row["answer_grade"] not in ALLOWED_GRADE]
    print("rows:", len(rows), "| bad status/grade:", len(bad))
    for row in bad[:10]:
        print("  BAD:", row["request_id"], row["status"], row["answer_grade"])

    if not ok or header != required or bad:
        sys.exit(1)


if __name__ == "__main__":
    main()
