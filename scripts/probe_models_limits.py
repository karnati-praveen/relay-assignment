"""Confirm per-model free-tier limits from response headers, for the models
we might use. This is Table 1 evidence."""
import json, urllib.request, os, time

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
kp = os.path.join(BASE, ".groq_key")
key = open(kp).read().strip() if os.path.exists(kp) else os.environ["GROQ_API_KEY"].strip()

for model in ["openai/gpt-oss-20b", "openai/gpt-oss-120b", "qwen/qwen3.8-27b"]:
    payload = {"model": model,
               "messages": [{"role": "user", "content": "Reply with the single word OK."}],
               "max_tokens": 5, "temperature": 0}
    req = urllib.request.Request(
        "https://api.groq.com/openai/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json",
                 "User-Agent": "Mozilla/5.0", "Accept": "application/json"},
    )
    t0 = time.time()
    try:
        r = urllib.request.urlopen(req, timeout=60)
        h = dict(r.headers)
        d = json.load(r)
        print(f"{model}: status 200 wall_ms={round((time.time()-t0)*1000,1)}")
        for k in sorted(h):
            if "ratelimit" in k.lower():
                print("   ", k, "=", h[k])
        print("    usage:", d.get("usage"))
    except urllib.error.HTTPError as e:
        print(f"{model}: HTTP {e.code}", e.read().decode("utf-8", "replace")[:300])
        for k, v in e.headers.items():
            if "ratelimit" in k.lower() or "retry" in k.lower():
                print("   ", k, "=", v)
    time.sleep(1)
