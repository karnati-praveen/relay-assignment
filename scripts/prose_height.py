"""Report how much vertical space report.md's prose occupies, in pages.

pagecount_prose.py says how many pages the rendered prose fills; this says how
far into the last page it runs, which is what tells you how much to cut.
"""
import os
import re

from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.lib.units import mm
from reportlab.platypus import Paragraph

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ss = getSampleStyleSheet()
body = ParagraphStyle("b", parent=ss["BodyText"], fontSize=9.5, leading=11, spaceAfter=1)
h2 = ParagraphStyle("h2", parent=ss["Heading2"], fontSize=10.5, leading=12, spaceBefore=3, spaceAfter=1)
h1 = ParagraphStyle("h1", parent=ss["Heading1"], fontSize=13, leading=15, spaceAfter=3)
h3 = ParagraphStyle("h3", parent=ss["Heading3"], fontSize=10, leading=12, spaceBefore=4, spaceAfter=2)

lines = open(os.path.join(BASE, "report.md"), encoding="utf-8").read().split("\n")
flow, buf = [], []


def flush():
    if not buf:
        return
    s = " ".join(buf)
    s = s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    s = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", s)
    s = re.sub(r"`(.+?)`", r"<font face='Courier'>\1</font>", s)
    flow.append(Paragraph(s, body))
    buf.clear()


i = 0
while i < len(lines):
    ln = lines[i]
    if ln.startswith("|"):
        flush()
        while i < len(lines) and lines[i].startswith("|"):
            i += 1
        flow.append(Paragraph("<i>[TABLE]</i>", body))
        continue
    if ln.startswith("### "):
        flush()
        flow.append(Paragraph(ln[4:], h3))
    elif ln.startswith("## "):
        flush()
        flow.append(Paragraph(ln[3:], h2))
    elif ln.startswith("# "):
        flush()
        flow.append(Paragraph(ln[2:], h1))
    elif ln.strip():
        buf.append(ln.strip())
    else:
        flush()
    i += 1
flush()

W = A4[0] - 32 * mm
H = A4[1] - 28 * mm
total = 0.0
for f in flow:
    _, h = f.wrap(W, H)
    total += h + getattr(f.style, "spaceAfter", 0) + getattr(f.style, "spaceBefore", 0)
print("flow height: %.0f pt" % total)
print("usable page: %.0f pt" % H)
print("PROSE PAGES: %.2f" % (total / H))
print("cut for 2 pages: %.0f pt (~%d words)" % (total - 2 * H, (total - 2 * H) / 6.0))
