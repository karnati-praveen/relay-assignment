"""Probe how many tokens a realistic pipeline prompt costs and what the
real service reports for TPM headroom. This is the measurement that decides
the whole architecture: on the free tier, TPM (8000) is far tighter than RPM.
"""
import json, urllib.request, time, os, random

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
kp = os.path.join(BASE, ".groq_key")
key = open(kp).read().strip() if os.path.exists(kp) else os.environ["GROQ_API_KEY"].strip()

# Load a slice of the Moshi paper to build a realistic retrieval prompt.
txt = open(os.path.join(BASE, "papers", "text", "moshi_2410.00037v1.txt"), encoding="utf-8").read()
words = txt.split()
random.seed(1)


def passage(nwords):
    i = random.randint(0, max(0, len(words) - nwords))
    return " ".join(words[i:i + nwords])


def call(payload):
    req = urllib.request.Request(
        "https://api.groq.com/openai/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json",
                 "User-Agent": "Mozilla/5.0", "Accept": "application/json"},
    )
    t0 = time.time()
    try:
        r = urllib.request.urlopen(req, timeout=90)
        return r.status, json.load(r), (time.time() - t0) * 1000, dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), (time.time() - t0) * 1000, dict(e.headers)


# Realistic pipeline prompt: system + 6 passages + question.
sys_msg = ("You answer questions about exactly three research papers, using ONLY the passages provided.\n"
           "Every factual claim must carry a citation of the form [paper p.N].\n"
           "If the passages do not contain the answer, reply exactly: NOT_IN_LIBRARY")
passages = "\n\n".join(f"[{i+1}] passage [moshi p.{i+1}]\n{passage(220)}" for i in range(6))
user = f"Passages:\n{passages}\n\nQuestion: How does Moshi handle streaming?"

payload = {"model": "openai/gpt-oss-20b",
           "messages": [{"role": "system", "content": sys_msg}, {"role": "user", "content": user}],
           "max_tokens": 700, "temperature": 0.2}

st, d, ms, h = call(payload)
print("status", st, "wall_ms", round(ms, 1))
if st == 200:
    u = d.get("usage", {})
    print("prompt_tokens:", u.get("prompt_tokens"), "completion_tokens:", u.get("completion_tokens"),
          "total:", u.get("total_tokens"))
    print("reasoning_tokens:", u.get("completion_tokens_details", {}).get("reasoning_tokens"))
    for k in h:
        if "ratelimit" in k.lower():
            print(" ", k, "=", h[k])
else:
    print(d[:600])
