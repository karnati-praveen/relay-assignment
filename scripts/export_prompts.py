"""Export this project's real session prompts, verbatim, from the T3 Code store.

The brief asks for the real conversation history, or the prompts if the tool
cannot export. T3 Code keeps the session in a local SQLite file; this reads it
read-only and writes one Markdown appendix. The API key that was pasted into
the chat once is replaced with [REDACTED].

Usage:
  python scripts/export_prompts.py --thread 878afe2e-... --out results/prompts.md
  python scripts/export_prompts.py --list          # show candidate threads
"""
import argparse
import json
import os
import re
import sqlite3

DB = os.path.join(os.path.expanduser("~"), ".t3", "userdata", "statev2.sqlite")
KEY_RE = re.compile(r"gsk_[A-Za-z0-9]{20,}")
UI_ONLY = {"continue", "/compact", "Background task completed."}


def connect():
    uri = "file:" + DB.replace("\\", "/") + "?mode=ro&immutable=1"
    return sqlite3.connect(uri, uri=True)


def threads(c):
    q = """
    select t.thread_id, t.title, t.updated_at,
           (select count(*) from orchestration_v2_projection_messages m
              where m.thread_id = t.thread_id and m.role = 'user')
    from orchestration_v2_projection_threads t
    where t.thread_id not like 'import:%'
    order by t.updated_at desc
    """
    return list(c.execute(q))


def prompts(c, thread):
    q = """
    select payload_json from orchestration_v2_projection_messages
    where thread_id = ? and role = 'user' order by created_at
    """
    out = []
    for (pj,) in c.execute(q, (thread,)):
        d = json.loads(pj)
        text = d.get("text") or ""
        atts = [a.get("name", "?") for a in (d.get("attachments") or [])]
        out.append((d.get("createdAt"), text, atts))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--thread", default="878afe2e-5da6-4fd7-8c01-45ea54ee4c7c")
    ap.add_argument("--out", default="")
    ap.add_argument("--list", action="store_true")
    a = ap.parse_args()

    c = connect()
    if a.list:
        for tid, title, upd, n in threads(c):
            print("%-40s u=%-3d %s | %s" % (tid, n, (title or "")[:50], upd))
        return

    rows = prompts(c, a.thread)
    n_ui = sum(1 for _, t, _ in rows if (t or "").strip() in UI_ONLY)
    lines = []
    lines.append("## Appendix 5: the prompts, as sent, in order (verbatim)\n")
    lines.append(
        "Exported read-only from the T3 Code session store with\n"
        "`scripts/export_prompts.py`, which prints exactly what was typed: typos,\n"
        "lower case, `/compact`, and pasted text included. %d entries are UI\n"
        "events rather than prompts and are marked. The API key pasted once in\n"
        "message 2 is shown as `[REDACTED]`.\n" % n_ui)
    for i, (at, text, atts) in enumerate(rows, 1):
        text = KEY_RE.sub("[REDACTED]", text)
        label = " _(UI event, not a prompt)_" if text.strip() in UI_ONLY else ""
        att = ("  \n_attached: %s_" % ", ".join(atts)) if atts else ""
        stamp = (at or "").replace("T", " ").replace(".000Z", "").replace("Z", " UTC")
        body = text.strip()
        if "\n" in body:
            quoted = "\n".join("> " + ln for ln in body.splitlines())
        else:
            quoted = "> " + body
        lines.append("**%d.** `%s`%s%s\n\n%s\n" % (i, stamp, label, att, quoted))
    out = "\n".join(lines)

    if a.out:
        with open(a.out, "w", encoding="utf-8", newline="\n") as f:
            f.write(out + "\n")
        print("wrote %s (%d prompts, %d chars)" % (a.out, len(rows), len(out)))
    else:
        print(out)


if __name__ == "__main__":
    main()
