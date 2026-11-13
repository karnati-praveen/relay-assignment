"""Render report.md to PDF-ish layouts and count pages.

No pandoc/wkhtmltopdf here, so we lay the text out with reportlab: a normal
layout (9.5pt) and a compact one (8.6pt, tighter margins). Tables render as real
tables. This estimates *layout* length, which is what the "2 pages of writing"
constraint is about.
"""
import re, os
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import getSampleStyleSheet, ParagraphStyle
from reportlab.lib.units import mm
from reportlab.lib import colors
from reportlab.platypus import (SimpleDocTemplate, Paragraph, Spacer, Table,
                                TableStyle)
from pypdf import PdfReader

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ss = getSampleStyleSheet()


def build(font, lead, gap, cellfont, celllead, margin, out, drop_tables=False):
    body = ParagraphStyle("body", parent=ss["BodyText"], fontSize=font,
                          leading=lead, spaceAfter=gap)
    h1 = ParagraphStyle("h1", parent=ss["Heading1"], fontSize=font + 4.5,
                        leading=lead + 3)
    h2 = ParagraphStyle("h2", parent=ss["Heading2"], fontSize=font + 1.5,
                        leading=lead + 1, spaceBefore=gap + 1, spaceAfter=2)
    h3 = ParagraphStyle("h3", parent=ss["Heading3"], fontSize=font + 0.5,
                        leading=lead, spaceBefore=2, spaceAfter=1)
    cell = ParagraphStyle("cell", parent=ss["BodyText"], fontSize=cellfont,
                          leading=celllead)
    head = ParagraphStyle("head", parent=cell, fontName="Helvetica-Bold")

    def mk_table(rows):
        # Drop the markdown separator line (|---|:--|). It is not data, and it is
        # not always at index 1 once blank/continued rows are considered, so
        # filter by content rather than by stride -- rows[::2] would silently
        # drop half the data rows and under-count the table's height.
        data_rows = [r for r in rows
                     if not set(r.strip().strip("|").replace(" ", "")) <= set("-:")]
        data = [[Paragraph(c.strip(), head if r == 0 else cell)
                 for c in ln.strip().strip("|").split("|")]
                for r, ln in enumerate(data_rows)]
        t = Table(data, repeatRows=1)
        t.setStyle(TableStyle([
            ("GRID", (0, 0), (-1, -1), 0.25, colors.grey),
            ("BACKGROUND", (0, 0), (-1, 0), colors.whitesmoke),
            ("VALIGN", (0, 0), (-1, -1), "TOP"),
            ("LEFTPADDING", (0, 0), (-1, -1), 2),
            ("RIGHTPADDING", (0, 0), (-1, -1), 2),
            ("TOPPADDING", (0, 0), (-1, -1), 1),
            ("BOTTOMPADDING", (0, 0), (-1, -1), 1),
        ]))
        return t

    lines = open(os.path.join(BASE, "report.md"), encoding="utf-8").read().split("\n")
    flow = []
    buf = []

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
        if ln.startswith("| "):
            flush()
            tbl = []
            while i < len(lines) and lines[i].startswith("|"):
                tbl.append(lines[i])
                i += 1
            if drop_tables:
                flow.append(Paragraph("<i>[TABLE omitted]</i>", body))
            else:
                flow.append(mk_table(tbl))
                flow.append(Spacer(1, 4))
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

    SimpleDocTemplate(out, pagesize=A4, topMargin=margin * mm,
                      bottomMargin=margin * mm, leftMargin=margin * mm,
                      rightMargin=margin * mm).build(flow)
    return len(PdfReader(out).pages)


normal = os.path.join(BASE, "results", "report_layout_estimate.pdf")
compact = os.path.join(BASE, "results", "report_layout_estimate_compact.pdf")
n = build(9.5, 12, 4, 6.4, 7.4, 16, normal)
c = build(8.6, 10.4, 2.6, 5.9, 6.8, 11, compact)
print("normal 9.5pt/16mm pages:", n)
print("compact 8.6pt/11mm pages:", c)

# Prose-only (tables collapsed): isolates what the mandatory tables cost.
p = build(8.6, 10.4, 2.6, 5.9, 6.8, 11,
          os.path.join(BASE, "results", "report_prose_only.pdf"), drop_tables=True)
print("compact prose-only pages:", p)