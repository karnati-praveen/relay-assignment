"""Re-export the session's prompts and replace Appendix 5 in the record.

The brief asks for the real conversation history, not a cleaned-up one, so the
appendix must be the export of the session as it actually stands when the
submission is staged — including the prompts sent after the narrative was
written. Doing that by hand invites a partial paste, so this truncates the
record at the appendix heading and appends a fresh export.

Usage (from the project root):
  python scripts/refresh_appendix5.py                 # rewrite in place
  python scripts/refresh_appendix5.py --check         # show counts, write nothing
"""
import argparse
import os
import re
import subprocess
import sys

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HIST = os.path.join(BASE, "results", "conversation_history.md")
EXPORT = os.path.join(BASE, "scripts", "export_prompts.py")
TMP = os.path.join(BASE, "results", "prompts.md")
HEAD = "## Appendix 5: the prompts, as sent, in order (verbatim)"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--thread", default="878afe2e-5da6-4fd7-8c01-45ea54ee4c7c")
    ap.add_argument("--check", action="store_true")
    a = ap.parse_args()

    r = subprocess.run([sys.executable, EXPORT, "--thread", a.thread,
                        "--out", TMP], capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit("export_prompts.py failed:\n" + r.stdout + r.stderr)
    export = open(TMP, encoding="utf-8").read().strip()
    if not export.startswith(HEAD):
        sys.exit("export does not start with the appendix heading")

    hist = open(HIST, encoding="utf-8").read()
    i = hist.find(HEAD)
    if i < 0:
        sys.exit("%s has no '%s' heading to replace" % (HIST, HEAD))
    body = hist[:i].rstrip() + "\n\n" + export + "\n"

    n_hist = len(re.findall(r"^\*\*\d+\.\*\*", hist, flags=re.M))
    n_new = len(re.findall(r"^\*\*\d+\.\*\*", export, flags=re.M))
    print("record: %d prompts before, %d after" % (n_hist, n_new))
    if a.check:
        print("check only: %s untouched" % os.path.relpath(HIST, BASE))
        return
    with open(HIST, "w", encoding="utf-8", newline="\n") as f:
        f.write(body)
    print("wrote %s (%d chars)" % (os.path.relpath(HIST, BASE), len(body)))


if __name__ == "__main__":
    main()
