# Vaani feedback

I looked at https://www.vaani.dev/ (platform, developers and pricing pages) and
the demo. Notes, in the order I actually hit them.

**What is immediately good.** The home page states the product in one line — "the
only voice AI harness that you'll ever need" — and then does something most
infrastructure pages do not: it lets you *hear* the thing before reading
anything. "Describe the call, hear the agent, then let it dial only inside the
window" is a concrete promise, and the live-call demo backs it. For an audio
product, an audio demo on the landing page is the right call, and it made me
understand what Vaani is faster than the platform page did. The "Launched #1 on
Product Hunt" badge is social proof I could verify, not a vague claim.

**Where I got stuck.** The site is a harness, not a model, and that distinction
took me two pages to work out. The landing page talks about the "world of Vaani
Voice" and "millions of calls", but what an engineer actually gets — the
orchestration, the turn-taking, the interruption handling, the tool calls, the
latency budget — is only really spelled out on `/platform` and `/developers`. If
I am evaluating this against my own pipeline, what I want in the first screen is
the *shape of the thing*: is it a managed agent runtime, a set of SDKs, or a
phone number plus a dashboard? I had to visit three pages to answer that.

**The latency question, which is the one that matters.** My own assignment is
about latency under a rate limit, so the first thing I looked for on a voice
platform was a latency number. The site's pitch implies real-time behaviour but
the specific number I care about — time to first audio byte — is not on the
landing page. For a product whose entire value is conversational timing, "how
fast do I hear the first word and how often does the agent talk over me" is the
number I would put next to the demo, the way Groq puts `queue_time` in its
response body. I found the p90 figure quoted in secondary coverage (roughly
450 ms) but not stated plainly on the page itself.

**Pricing.** The page links to `/pricing` but the pricing model I could see is
directional rather than dimensional — it is hard to answer "what does one
thousand five-minute calls cost me" from the landing flow, which is the question
an engineer sizing a pilot actually asks. The free tier exists and is clearly
labelled, which is good; the unit economics are the gap.

**What I would change.** Three things, in priority order. (1) Put one real
latency number on the home page — TTFA p50 and the interruption rate — because
that is the product. (2) Add a single paragraph above the fold saying what I
install: SDK, hosted runtime, or both, and in which languages. (3) On the demo,
show the *transcript with timings* next to the audio, so I can see the turn
boundaries rather than only hear them; that is the same instinct as shipping the
call recording, which Vaani already does well.

**The honest caveat.** I evaluated Vaani as a text-and-pricing reader, not as a
customer with live phone traffic, so my feedback is about the first thirty
minutes of an evaluation, which is exactly the window where a landing page wins
or loses a technical buyer. I never placed a call, so I cannot speak to call
quality, and I did not get far enough to hit a rate limit or a quota wall of my
own — which, given how much of my own week was spent fighting someone else's
limits, is probably the fairest thing I can say about Vaani's onboarding: it did
not put a wall in front of me.
