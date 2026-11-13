"""Download the three assigned papers at the exact pinned versions.

The submission ships `papers/text/*.txt` (the extracted text the retriever
actually reads), so nothing has to be downloaded to run the pipeline. This
script restores the PDFs for anyone who wants to check a cited page by eye.

arXiv serves a versioned PDF at /pdf/<id><version>; the versions are pinned in
PAPERS because the page numbers in `report.md` and in every answer's citation
belong to those versions, not to whatever is current.

Usage:
  python scripts/fetch_papers.py            # skip files already present
  python scripts/fetch_papers.py --force    # re-download everything
"""
import argparse
import os
import sys
import urllib.request

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(BASE, "papers")

PAPERS = [
    ("attention_1706.03762v7.pdf", "1706.03762v7"),
    ("moshi_2410.00037v1.pdf", "2410.00037v1"),
    ("chunking_2507.07955v2.pdf", "2507.07955v2"),
]

URL = "https://arxiv.org/pdf/%s"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--force", action="store_true")
    a = ap.parse_args()
    os.makedirs(OUT, exist_ok=True)

    for name, version in PAPERS:
        dest = os.path.join(OUT, name)
        if os.path.exists(dest) and os.path.getsize(dest) > 100_000 and not a.force:
            print("present  %s (%d bytes)" % (name, os.path.getsize(dest)))
            continue
        url = URL % version
        print("fetching %s" % url)
        req = urllib.request.Request(url, headers={"User-Agent": "relay-fetch/1.0"})
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                data = r.read()
        except Exception as e:                      # noqa: BLE001 - report, don't trace
            sys.exit("failed to fetch %s: %s" % (url, e))
        if not data.startswith(b"%PDF"):
            sys.exit("%s did not return a PDF (%d bytes)" % (url, len(data)))
        with open(dest, "wb") as f:
            f.write(data)
        print("wrote    %s (%d bytes)" % (name, len(data)))


if __name__ == "__main__":
    main()
