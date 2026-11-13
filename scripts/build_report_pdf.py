"""Render report.md as report.pdf with the brief's normal 9.5pt body.

The page-count scripts estimate layout; this one produces the file that goes in
the submission folder. Tables are real tables, not screenshots.

Usage:
  python scripts/build_report_pdf.py
"""
import os
import re

from reportlab.lib import colors
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.lib.units import mm
from reportlab.platypus import (Paragraph, SimpleDocTemplate, Spacer, Table,
                                TableStyle)
from pypdf import PdfReader

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(BASE, "report.md")
OUT = os.path.join(BASE, "report.pdf")

ss = getSampleStyleSheet()
# Layout knobs. The write-up must fit the brief's two pages, so these are kept
# in one place and the page count is checked after every change. Body text
# stays at a readable size; the tables are the densest part.
BODY_PT, BODY_LEAD = 8.2, 9.35
CELL_PT, CELL_LEAD = 5.6, 6.3
MARGIN_V, MARGIN_H = 10, 11            # mm, top/bottom and left/right

body = ParagraphStyle("body", parent=ss["BodyText"], fontName="Helvetica",
                      fontSize=BODY_PT, leading=BODY_LEAD, spaceAfter=0.5)
h1 = ParagraphStyle("h1", parent=ss["Heading1"], fontName="Helvetica-Bold",
                    fontSize=13, leading=15.5, spaceAfter=3)
h2 = ParagraphStyle("h2", parent=ss["Heading2"], fontName="Helvetica-Bold",
                    fontSize=9.9, leading=11.4, spaceBefore=2.5, spaceAfter=0.5)
h3 = ParagraphStyle("h3", parent=ss["Heading3"], fontName="Helvetica-Bold",
                    fontSize=9.2, leading=11, spaceBefore=3, spaceAfter=1.5)
cell = ParagraphStyle("cell", parent=ss["BodyText"], fontName="Helvetica",
                      fontSize=CELL_PT, leading=CELL_LEAD)
head = ParagraphStyle("head", parent=cell, fontName="Helvetica-Bold")
body_item = ParagraphStyle("body_item", parent=body, leftIndent=12,
                           bulletIndent=2, spaceAfter=1,
                           bulletFontName="Helvetica", bulletFontSize=BODY_PT)


def inline(s):
    s = s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    s = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", s)
    s = re.sub(r"\*(.+?)\*", r"<i>\1</i>", s)
    s = re.sub(r"`(.+?)`", r"<font face='Courier'>\1</font>", s)
    return s


def make_table(rows):
    def is_rule(ln):
        cells = [c.strip() for c in ln.strip().strip("|").split("|")]
        return bool(cells) and all(c and set(c) <= set("-:") for c in cells)

    data_rows = [r for r in rows if not is_rule(r)]
    data = [[Paragraph(inline(c.strip()), head if i == 0 else cell)
             for c in ln.strip().strip("|").split("|")]
            for i, ln in enumerate(data_rows)]
    # Explicit column widths: with no widths, reportlab sizes the row from a
    # different column width than it finally draws, so a wrapped header can
    # cross its own rule. Weights follow the longest cell, so the prose columns
    # get room and the numeric ones stay narrow.
    ncols = max(len(r) for r in data)
    avail = A4[0] - (MARGIN_H * 2) * mm
    weights = []
    for c in range(ncols):
        longest = max((len(r[c].getPlainText()) if c < len(r) else 0) for r in data)
        weights.append(min(max(longest, 8), 34))
    total = float(sum(weights))
    widths = [avail * w / total for w in weights]
    t = Table(data, repeatRows=1, colWidths=widths)
    t.setStyle(TableStyle([
        ("GRID", (0, 0), (-1, -1), 0.25, colors.grey),
        ("BACKGROUND", (0, 0), (-1, 0), colors.whitesmoke),
        ("VALIGN", (0, 0), (-1, -1), "TOP"),
        ("LEFTPADDING", (0, 0), (-1, -1), 2),
        ("RIGHTPADDING", (0, 0), (-1, -1), 2),
        ("TOPPADDING", (0, 0), (-1, -1), 1.5),
        ("BOTTOMPADDING", (0, 0), (-1, -1), 1.5),
    ]))
    return t


def main():
    lines = open(SRC, encoding="utf-8").read().split("\n")
    flow, buf = [], []
    item = []          # an ordered/unordered list item, kept out of `buf`
    list_re = re.compile(r"^(\s*)((?:[-*])|(?:\d+\.))\s+(.*)$")

    def flush():
        if not buf:
            return
        text = inline(" ".join(buf))
        flow.append(Paragraph(text, body))
        buf.clear()

    def flush_item():
        if not item:
            return
        marker, rest = item[0], " ".join(item[1:])
        if marker in ("-", "*"):
            marker = "\u2022"
        flow.append(Paragraph(inline(rest), body_item, bulletText=marker))
        item.clear()

    i = 0
    while i < len(lines):
        ln = lines[i]
        if ln.startswith("| "):
            flush()
            flush_item()
            tbl = []
            while i < len(lines) and lines[i].startswith("|"):
                tbl.append(lines[i])
                i += 1
            flow.append(make_table(tbl))
            flow.append(Spacer(1, 2))
            continue
        if ln.startswith("### "):
            flush()
            flush_item()
            flow.append(Paragraph(inline(ln[4:]), h3))
            i += 1
            continue
        if ln.startswith("## "):
            flush()
            flush_item()
            flow.append(Paragraph(inline(ln[3:]), h2))
            i += 1
            continue
        if ln.startswith("# "):
            flush()
            flush_item()
            flow.append(Paragraph(inline(ln[2:]), h1))
            i += 1
            continue
        m = list_re.match(ln)
        if m:
            # A new marker closes the previous item; the numbered list in the
            # retrieval section is four hard-wrapped lines of one item, so the
            # continuation lines must join it rather than start a paragraph.
            flush()
            flush_item()
            item.extend([m.group(2) + (" " if m.group(2)[0].isdigit() else ""),
                         m.group(3)])
            i += 1
            continue
        if ln.startswith("> "):
            # A blockquote is hard-wrapped over several source lines; join them
            # into one italic paragraph instead of italicising only the first.
            flush()
            flush_item()
            quote = []
            while i < len(lines) and lines[i].startswith("> "):
                quote.append(lines[i][2:])
                i += 1
            flow.append(Paragraph("<i>" + inline(" ".join(quote)) + "</i>", body))
            continue
        elif ln.strip():
            if item:
                item.append(ln.strip())
            else:
                buf.append(ln.strip())
        else:
            flush()
            flush_item()
        i += 1
    flush()
    flush_item()

    SimpleDocTemplate(OUT, pagesize=A4, topMargin=MARGIN_V * mm, bottomMargin=MARGIN_V * mm,
                      leftMargin=MARGIN_H * mm, rightMargin=MARGIN_H * mm,
                      title="The Relay").build(flow)
    print("wrote", OUT, "pages:", len(PdfReader(OUT).pages))


if __name__ == "__main__":
    main()
