"""Check that every pattern finalize_report.py patches still matches once.

finalize_report.py refuses on a miss; this reports all misses at once, after a
hand edit to report.md, without needing the sweep files to exist.

One miss is expected before finalize runs: the retrieval-ceiling pattern only
matches after TBD_CHEAP_ANS has been substituted to a number.
"""
import re

pats = [
    r"(only \*\*)[\d.]+(%\*\* were answered)",
    r"(refused locally in \*\*)[\d.]+( ms\*\* for \*\*zero tokens\*\*)",
    r"(flat \()[\d,]+–[\d,]+(\) and 1,000 answers cost ≈\*\*\$)[\d.]+",
    r"(timeouts\*\* in )[\d,]+( requests)",
    r"(\*\*)[\d,]+( of )[\d,]+(\*\* were given up)",
    r"(\*\*)[\d,]+(\*\* by admission control and \*\*)[\d,]+(\*\* by the daily budget)",
    r"(pointers in\s+\*\*)[\d.]+(\s+ms p50 / )[\d.]+(\s+ms p95\*\*)",
    r"(\*\*)[\d,]+(\*\* of )[\d,]+(\s+requests,\s+returning local pointers)",
    r"(\*\*)[\d,]+(\*\*\s+more were dropped by the\s+daily budget,\s+\*\*)[\d,]+(\s+of\s+)[\d,]+(\s+in all\*\*)",
    r"TBD_P50",
    r"TBD_TTFT",
    r"TBD_REQ_TTFTPCT",
    r"TBD_REQ_WAITPCT",
    r"TBD_REQ_RETRYPCT",
    r"TBD_REQ_WORKPCT",
    r"TBD_REQ_TOTAL",
    r"TBD_REQ_WAIT\b",
    r"TBD_REQ_RETRY\b",
    r"TBD_REQ_WORK\b",
    r"TBD_REQ_TTFT\b",
    r"TBD_REQ\b",
    r"TBD_CORRECT",
    r"TBD_WRONG",
    r"TBD_REFW\b",
    r"TBD_P120_CORRECT",
    r"TBD_P120_TOK",
    r"TBD_CHEAP_ANS",
    r"TBD_REAL_TOK",
    r"TBD_FACTOR",
    r"(one answer costs\s+\*\*)[\d,]+( tokens\*\* on the real service)",
    r"(\*\*)[\d.]+(% of a minute's budget\*\*)",
    r"(buys \*\*)[\d.]+(\s+answers, not 30\*\*)",
    r"(are the stand-in's character count\s*\()[\d,]*\d"
    r"(\s+tokens/answer, within )[\d.]+(% of the real )[\d,]*\d",
    r"(all 20 questions: )[\d,]+( tokens/answer)",
    r"(so )[\d,]+( tokens/answer meant waiting)",
    r"(ceiling from )[\d.]+(\s+to\s+)[\d.]+",
    r"(waiting \*\*)\d+(?:[-\u2013]\d+)?(\s+minutes per question\*\*)",
    r"(all six load shapes \()[\d,]+(\s+requests\))",
    r"(\*\*)[\d.]+( ms\*\* median)",
]
text = open("report.md", encoding="utf-8").read()
# TBD_REFW appears twice: once in Table 3's prose, once in the assistant
# section, and finalize replaces every copy.
EXPECT = {r"TBD_REFW\b": 2}
bad = 0
for p in pats:
    n = len(re.findall(p, text))
    want = EXPECT.get(p, 1)
    if n != want:
        bad += 1
        print("MISS %dx  %s" % (n, p))
print("patterns:", len(pats), "misses:", bad)
