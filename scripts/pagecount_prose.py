"""Render report.md prose only (tables replaced by a placeholder) to see how
many pages the *writing* alone occupies, separately from the mandatory tables.
"""
import re, os
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import getSampleStyleSheet, ParagraphStyle
from reportlab.lib.units import mm
from reportlab.platypus import SimpleDocTemplate, Paragraph

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ss = getSampleStyleSheet()
body = ParagraphStyle("b", parent=ss["BodyText"], fontSize=9.5, leading=11, spaceAfter=1)
h2 = ParagraphStyle("h2", parent=ss["Heading2"], fontSize=10.5, leading=12, spaceBefore=3, spaceAfter=1)
h1 = ParagraphStyle("h1", parent=ss["Heading1"], fontSize=13, leading=15, spaceAfter=3)
h3 = ParagraphStyle("h3", parent=ss["Heading3"], fontSize=10, leading=12, spaceBefore=4, spaceAfter=2)

t = open(os.path.join(BASE, "report.md"), encoding="utf-8").read().split("\n")
flow = []
buf = []


def flush():
    """Join the hard-wrapped source lines of one paragraph before laying it
    out, exactly as scripts/pagecount.py does -- otherwise each ~80-column
    source line becomes its own paragraph and the page count is inflated."""
    if not buf:
        return
    s = " ".join(buf)
    s = s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    s = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", s)
    s = re.sub(r"`(.+?)`", r"<font face='Courier'>\1</font>", s)
    flow.append(Paragraph(s, body))
    buf.clear()


i = 0
while i < len(t):
    ln = t[i]
    if ln.startswith("|"):
        flush()
        while i < len(t) and t[i].startswith("|"):
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

out = os.path.join(BASE, "results", "report_prose_only.pdf")
SimpleDocTemplate(out, pagesize=A4, topMargin=14*mm, bottomMargin=14*mm,
                  leftMargin=16*mm, rightMargin=16*mm).build(flow)
from pypdf import PdfReader
print("PROSE-ONLY PAGES:", len(PdfReader(out).pages))
