// Package pipeline is the Relay answer pipeline: understand -> retrieve ->
// decide -> draft -> check, with a bounded revise loop and a retrieval tool.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"relay/internal/limits"
	"relay/internal/papers"
	"relay/internal/provider"
	"relay/internal/retrieval"
)

// Config controls pipeline behaviour.
type Config struct {
	Model       string // model used for the expensive draft call
	RouterModel string // cheap model used for the plan/understand step
	CheckModel  string // model used for the verification step
	Settings    retrieval.Settings
	MaxTokens   int
	Temperature float64

	// Per-step completion budgets. Reasoning models spend part of the budget
	// before emitting the structured answer, so the planner and checker need
	// more than the visible JSON requires; a budget that is too small makes
	// Groq fail JSON validation inside an HTTP 200 stream.
	RouterMaxTokens int
	CheckMaxTokens  int
	MaxRevisions    int     // extra loops of the check->draft step
	MinCoverage     float64 // retrieval coverage below which we refuse outright
	RefuseOnWeak    bool
	PromptVersion   string

	// Post-hoc repair switches, all off by default. The measured sweeps keep
	// the configuration they were measured under; a later run can turn these
	// on and be compared like for like, because each one is tied to a named
	// failure in results/quality_manual.csv.
	//
	// ScopeFromQuestion: q20 was refused because the planner guessed the wrong
	// paper ("attention") and validCitationSet then rejected a correct
	// [chunking p.1] citation as out of scope. When the question itself names a
	// paper, that name defines the scope; a planner guess must not.
	ScopeFromQuestion bool
	// LenientCheck: q07 and q08 held the right fact on the right page and were
	// refused because the checker demanded a citation for every sentence
	// rather than for every factual claim. The gate keeps its job — an
	// uncited claim or a contradicted claim still fails — without rejecting an
	// answer whose supporting sentence is already cited.
	LenientCheck bool

	// MaxQueueWait bounds how long a request may wait for provider capacity.
	// Waiting forever is not kindness: the person has already given up. When
	// the next admissible window is further out than this, the request is shed
	// immediately with an honest "busy" reply instead of hanging.
	MaxQueueWait time.Duration

	// Retry policy.
	MaxRetries   int           // attempts beyond the first
	RetryBase    time.Duration // first backoff
	RetryMax     time.Duration // cap on a single backoff
	RetryOn429   bool
	RetryOnError bool
}

// DefaultConfig returns sensible defaults.
func DefaultConfig(model string) Config {
	return Config{
		Model:           model,
		RouterModel:     "qwen/qwen3.8-27b",
		CheckModel:      "openai/gpt-oss-20b",
		Settings:        retrieval.Settings{PieceWords: 220, Overlap: 40, Pieces: 6, Rerank: true},
		MaxTokens:       700,
		Temperature:     0.2,
		RouterMaxTokens: 300,
		// The checker reasons before it answers, and it now reads the full
		// passages (~2,400 prompt tokens, not the 590 of 200-character
		// excerpts): on the excerpt-sized prompt it already spent 474 tokens
		// reasoning. A budget that runs out before the verdict turns a correct
		// answer into a refusal, which is the failure this whole step exists to
		// avoid, so it gets room to think.
		CheckMaxTokens: 1024,
		MaxRevisions:   1,
		MinCoverage:    0.20,
		RefuseOnWeak:   true,
		PromptVersion:  "v3",
		MaxQueueWait:   8 * time.Second,
		MaxRetries:     3,
		RetryBase:      500 * time.Millisecond,
		RetryMax:       12 * time.Second,
		RetryOn429:     true,
		RetryOnError:   true,
	}
}

// Trace records per-step timing for one request, so latency can be split into
// waiting / working / retrying.
type Trace struct {
	Steps        []StepTiming `json:"steps"`
	Calls        []CallTrace  `json:"calls,omitempty"`
	TokensIn     int          `json:"tokens_in"`
	TokensOut    int          `json:"tokens_out"`
	Retries      int          `json:"retries"`
	RetrievalK   int          `json:"retrieval_k"`
	HitPapers    []string     `json:"hit_papers"`
	Coverage     float64      `json:"coverage"`
	Refused      bool         `json:"refused"`
	DayExhausted bool         `json:"day_exhausted"`
	// Shed is true when admission control refused to start (or continue) a
	// request because the next capacity window was further out than
	// MaxQueueWait. A shed request costs no provider call and returns in
	// milliseconds, which is the honest alternative to a 90-second hang.
	Shed  bool   `json:"shed,omitempty"`
	Grade string `json:"-"`

	// held is the request-level admission reservation: the whole request is
	// admitted once, before its first model call, and settled against actual
	// usage when it ends. Per-call admission let a 60-request burst buy 30
	// cheap planner calls and then afford no draft, so every request started
	// work it could not finish. Unserialised: it is bookkeeping, not a result.
	held *hold
}

// hold is one request's reserved capacity.
type hold struct {
	decision limits.Decision
	tokens   int
}

// CallTrace is the auditable record of one provider attempt. In particular it
// proves that planning, drafting and checking used the models claimed in the
// configuration rather than a provider-side default.
type CallTrace struct {
	Step         string  `json:"step"`
	Model        string  `json:"model"`
	Attempt      int     `json:"attempt"`
	Status       string  `json:"status"`
	TTFTMs       float64 `json:"ttft_ms"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	RequestID    string  `json:"request_id,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
	StreamEvents int     `json:"stream_events,omitempty"`
	Err          string  `json:"error,omitempty"`
	Head         string  `json:"response_head,omitempty"`
	Tail         string  `json:"response_tail,omitempty"`
}

// WaitingMs, RetryingMs and WorkingMs are three *disjoint* buckets: their sum
// is the request's inside-the-pipeline wall time. Waiting is admission-control
// waiting; retrying is retry backoff; working is model and tool time. A work
// step's own duration is measured net of any wait that happened inside it (see
// the `w0` bookkeeping in Run), so nothing is counted twice.
func (t Trace) WaitingMs() float64 {
	var w float64
	for _, s := range t.Steps {
		if s.Kind == "wait" && !strings.Contains(s.Name, "retrybackoff") {
			w += s.Ms
		}
	}
	return w
}

// rawWaitMs is every wait step, including retry backoff. Run subtracts the
// growth of this total from a work step's elapsed time.
func (t Trace) rawWaitMs() float64 {
	var w float64
	for _, s := range t.Steps {
		if s.Kind == "wait" {
			w += s.Ms
		}
	}
	return w
}

func (t Trace) WorkingMs() float64 {
	var w float64
	for _, s := range t.Steps {
		if s.Kind == "work" {
			w += s.Ms
		}
	}
	return w
}

// RetryingMs is the part of waiting that is retry backoff rather than
// admission-control waiting.
func (t Trace) RetryingMs() float64 {
	var w float64
	for _, s := range t.Steps {
		if s.Kind == "wait" && strings.Contains(s.Name, "retrybackoff") {
			w += s.Ms
		}
	}
	return w
}

// StepTiming is one step's wall time and classification.
type StepTiming struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"` // "work" (model/tool) or "wait" (backoff)
	Ms        float64 `json:"ms"`
	Detail    string  `json:"detail,omitempty"`
	Attempt   int     `json:"attempt,omitempty"`
	TokensIn  int     `json:"tokens_in,omitempty"`
	TokensOut int     `json:"tokens_out,omitempty"`
}

// Answer is the pipeline output.
type Answer struct {
	Text      string
	Citations []Citation
	Refused   bool
	Reason    string
	Status    string // ok, rate_limited, timeout, error
	Trace     Trace
	TTFTMs    float64
	TotalMs   float64
}

// Citation points at a specific passage.
type Citation struct {
	ChunkID string
	PaperID string
	Page    int
	Section string
}

// Pipeline holds the immutable index and settings.
type Pipeline struct {
	Index *retrieval.Index
	Cfg   Config
	Lib   *papers.Library

	// Bucket is the optional token/request admission gate. When set, the
	// pipeline waits before sending rather than collecting a 429, and stops
	// immediately when the daily budget is gone.
	Bucket *limits.Bucket
}

// SetBucket attaches an admission-control bucket.
func (p *Pipeline) SetBucket(b *limits.Bucket) { p.Bucket = b }

// callWithRetry wraps one model call with the retry policy. Every wait is
// recorded in the trace as a "wait" step, so retry time is separable from work
// time for the "where the time goes" analysis. It returns the last status.
func (p *Pipeline) callWithRetry(ctx context.Context, prov provider.Provider, req provider.Request, tr *Trace, stepName string) (provider.Result, string) {
	var res provider.Result
	attempt := 0
	for {
		// Admission control reserves the actual future window. Concurrent
		// callers therefore receive different slots instead of all sleeping to
		// the same window boundary and creating a second burst.
		//
		// The reservation is made once for the whole request, on its first
		// model call, and settled in Run against the request's total usage.
		// Reserving per call looked fairer but meant a burst could admit
		// thirty planner calls and no complete answer.
		if p.Bucket != nil && tr.held == nil {
			est := p.requestEstimate()
			admission := p.Bucket.Admit(est)
			if !admission.Allow {
				// A call bigger than a whole window can never run; that is a
				// capacity refusal, not an exhausted daily budget.
				if admission.TooLarge {
					tr.Shed = true
					tr.Steps = append(tr.Steps, StepTiming{
						Name: stepName + "-shed", Kind: "shed", Detail: admission.Reason,
					})
					return provider.Result{Status: "busy",
						Err: fmt.Errorf("a request this large cannot fit in one minute's token budget")}, "busy"
				}
				tr.DayExhausted = true
				tr.Steps = append(tr.Steps, StepTiming{
					Name: stepName + "-admission", Kind: "wait",
					Ms: float64(admission.RetryAfter.Microseconds()) / 1000.0, Detail: admission.Reason,
				})
				return provider.Result{Status: "rate_limited",
					Err: fmt.Errorf("daily budget exhausted")}, "rate_limited"
			}
			// Shed rather than park. Reserving a slot ten minutes out and
			// making the person wait for it is not queueing, it is a slower
			// refusal. If the earliest slot is beyond MaxQueueWait, release
			// the reservation and say so immediately.
			if admission.Wait > p.Cfg.MaxQueueWait {
				p.Bucket.Observe(admission, est, 0) // release the unused reservation
				tr.Shed = true
				tr.Steps = append(tr.Steps, StepTiming{
					Name: stepName + "-shed", Kind: "shed",
					Detail: fmt.Sprintf("next slot %s > max queue wait %s",
						admission.Wait.Round(time.Millisecond), p.Cfg.MaxQueueWait),
				})
				return provider.Result{Status: "busy",
					Err: fmt.Errorf("at capacity: next token window is %s away",
						admission.Wait.Round(time.Millisecond))}, "busy"
			}
			if admission.Wait > 0 {
				tr.Steps = append(tr.Steps, StepTiming{
					Name: stepName + "-admission", Kind: "wait",
					Ms: float64(admission.Wait.Microseconds()) / 1000.0, Detail: admission.Reason,
				})
				if !limits.Wait(ctx, admission) {
					p.Bucket.Observe(admission, est, 0) // release the unused reservation
					return provider.Result{Status: "timeout", Err: ctx.Err()}, "timeout"
				}
			}
			tr.held = &hold{decision: admission, tokens: est}
		}

		res = prov.Chat(ctx, req)
		addUsage(tr, res.Usage)
		tr.Calls = append(tr.Calls, CallTrace{
			Step: stepName, Model: req.Model, Attempt: attempt + 1,
			Status: res.Status, TTFTMs: res.TTFTMs,
			TokensIn: res.Usage.PromptTokens, TokensOut: res.Usage.CompletionTokens,
			RequestID: res.RequestID, FinishReason: res.FinishReason,
			StreamEvents: res.StreamEvents, Err: formatErr(res.Err),
			Head: res.ResponseHead, Tail: res.ResponseTail,
		})
		if res.Status == "rate_limited" && isDailyLimit(res) {
			tr.DayExhausted = true
			return res, res.Status
		}
		// A successful HTTP response with no assistant content is not an answer.
		// On reasoning models this can happen when the whole completion budget
		// is consumed internally; retry it instead of checking or sending an
		// empty reply.
		emptyOutput := res.Status == "ok" && strings.TrimSpace(res.Text) == "" && len(res.ToolCalls) == 0
		if res.Status == "ok" && !emptyOutput {
			res.Attempts = attempt + 1
			return res, "ok"
		}
		if emptyOutput {
			res.Status = "error"
			res.Err = fmt.Errorf("empty model output")
		}
		retryable := !permanentError(res) &&
			((res.Status == "rate_limited" && p.Cfg.RetryOn429) ||
				(res.Status == "error" && p.Cfg.RetryOnError))
		if !retryable || attempt >= p.Cfg.MaxRetries {
			res.Attempts = attempt + 1
			return res, res.Status
		}
		// json_validate_failed means the model ran out of completion budget
		// before it could emit schema-valid JSON. Retrying the identical
		// request would spend the same budget and fail the same way, so give
		// the retry more room (bounded) instead of looping.
		if res.Err != nil && strings.Contains(res.Err.Error(), "json_validate_failed") {
			if req.MaxTokens < 2048 {
				req.MaxTokens *= 2
			}
		}
		// Backoff: honor retry-after when present and reasonable.
		wait := p.Cfg.RetryBase * time.Duration(1<<attempt)
		if res.RL.RetryAfter != "" {
			if secs, err := strconv.ParseFloat(strings.TrimSuffix(res.RL.RetryAfter, "s"), 64); err == nil && secs > 0 {
				wait = time.Duration(secs * float64(time.Second))
			}
		}
		if wait > p.Cfg.RetryMax {
			wait = p.Cfg.RetryMax
		}
		tr.Retries++
		tr.Steps = append(tr.Steps, StepTiming{
			Name: stepName + "-retrybackoff", Kind: "wait",
			Ms: float64(wait.Microseconds()) / 1000.0, Attempt: attempt + 1,
			Detail: res.Status,
		})
		select {
		case <-ctx.Done():
			return res, "timeout"
		case <-time.After(wait):
		}
		attempt++
	}
}

// requestEstimate sizes the single reservation a request makes. It is not the
// sum of per-call maxima; it is the prompts that actually dominate the cost:
// the draft (k passages plus its completion) and the check.
//
// The check term changed shape when the checker stopped reading 200-character
// excerpts and started reading whole passages: its prompt is now as large as
// the drafter's, and on the real service gpt-oss spent ~1,450 reasoning tokens
// before emitting a verdict — and max_tokens does not cap those, a 1,024 cap
// produced 1,458 completion tokens. So both terms are measured, not assumed.
//
// Measured end to end at p220_k6: 6,123 tokens (understand 80, draft 2,526,
// check 3,517). This returns 6,286. It covers one draft plus one check, which
// is the expected path; a revision costs a second pair and deliberately shows
// up as an overrun in the trace rather than being hidden in a fat reservation
// that would halve admission throughput for every request.
func (p *Pipeline) requestEstimate() int {
	words := p.Cfg.Settings.Pieces * p.Cfg.Settings.PieceWords
	passages := words * 4 / 3 // ~1.33 tokens per word, measured on these papers
	draft := passages + 300 + minInt(p.Cfg.MaxTokens*2/3, 512)
	check := passages + 300 + 1500 // full passages + the checker's reasoning
	return 200 + draft + check
}

// EstimateRequestTokens exposes the whole-request reservation so a measurement
// harness can pace itself across a daily allowance instead of discovering the
// limit on a 429.
func (p *Pipeline) EstimateRequestTokens() int { return p.requestEstimate() }

// permanentError is true for a deterministic client error: the same request
// cannot succeed on a retry, so retrying only turns an honest failure into a
// slow one. 429 is handled before this point (it is rate_limited, not error,
// and it is retryable).
func permanentError(res provider.Result) bool {
	return res.HTTPStatus >= 400 && res.HTTPStatus < 500 && res.HTTPStatus != 429
}

func isDailyLimit(res provider.Result) bool {
	if res.Err == nil {
		return false
	}
	msg := strings.ToLower(res.Err.Error())
	return strings.Contains(msg, "per day") || strings.Contains(msg, "daily") ||
		strings.Contains(msg, "(tpd)") || strings.Contains(msg, "(rpd)")
}

func formatErr(err error) string {
	if err == nil {
		return ""
	}
	return truncate(sanitize(err.Error()), 300)
}

// New builds a pipeline over the given library.
func New(lib *papers.Library, cfg Config) *Pipeline {
	if cfg.MaxQueueWait <= 0 {
		cfg.MaxQueueWait = 8 * time.Second
	}
	lib.ChunkAll(cfg.Settings.PieceWords, cfg.Settings.Overlap)
	ix := retrieval.BuildIndex(lib.Chunks)
	return &Pipeline{Index: ix, Cfg: cfg, Lib: lib}
}

// searchTool is the schema of the one tool the pipeline executes. The planner
// supplies its arguments (query + paper), invokeTool validates them, and the
// retrieval index does the work; the schema is what makes the boundary typed.
var searchTool = provider.Tool{
	Name:        "search_library",
	Description: "Search the assigned arXiv paper library (Attention Is All You Need v7, Moshi v1, Dynamic Chunking v2) for passages matching a query. Returns ranked passages with paper, page and section.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "search query"},
			"paper": map[string]any{"type": "string", "description": "optional paper filter: attention|moshi|chunking"},
		},
		"required": []string{"query"},
	},
}

// Run executes the pipeline for one question. It is intentionally explicit so
// every step's cost is measurable.
func (p *Pipeline) Run(ctx context.Context, q string, prov provider.Provider, streamCB func(string)) Answer {
	t0 := time.Now()
	var tr Trace
	// One reservation per request, settled against what it actually spent.
	// The defer runs on every return path, including free local refusals that
	// never made a model call (tr.held stays nil and nothing is settled).
	if p.Bucket != nil {
		defer func() {
			if h := tr.held; h != nil {
				p.Bucket.Observe(h.decision, h.tokens, tr.TokensIn+tr.TokensOut)
			}
		}()
	}
	add := func(name, kind string, ms float64, attempt int, detail string) {
		tr.Steps = append(tr.Steps, StepTiming{Name: name, Kind: kind, Ms: ms, Attempt: attempt, Detail: detail})
	}
	// usageStep records how many tokens the most recent step consumed.
	lastIn, lastOut := 0, 0
	usageStep := func(name string) {
		tr.Steps = append(tr.Steps, StepTiming{
			Name: name + "-usage", Kind: "usage", TokensIn: lastIn, TokensOut: lastOut,
		})
	}
	status := "ok"

	// ---- Step 1: UNDERSTAND (model call) --------------------------------
	uStart, uWait := time.Now(), tr.rawWaitMs()
	beforeIn, beforeOut := tr.TokensIn, tr.TokensOut
	understood, uStatus := p.understand(ctx, q, prov, &tr)
	lastIn, lastOut = tr.TokensIn-beforeIn, tr.TokensOut-beforeOut
	add("understand", "work", ms(uStart)-(tr.rawWaitMs()-uWait), 0, understood.plan)
	usageStep("understand")
	if uStatus != "ok" {
		status = uStatus
	}

	// ---- Step 2: RETRIEVE (tool call) -----------------------------------
	// The planner produced the arguments; the tool schema bounds them and
	// invokeTool is the one place that executes a tool. A failure here is
	// fail-closed: without evidence there is nothing to verify.
	rStart := time.Now()
	toolArgs, _ := json.Marshal(searchArgs{Query: understood.query, Paper: understood.paper})
	hits, coverage, toolErr := p.invokeTool(searchTool.Name, toolArgs)
	if toolErr != nil {
		tr.Refused = true
		ans := Answer{Text: refuseText("retrieval tool failed: " + toolErr.Error()),
			Refused: true, Reason: toolErr.Error(), Trace: tr, Status: status, TotalMs: ms(t0)}
		if streamCB != nil {
			streamCB(ans.Text)
		}
		return ans
	}
	tr.RetrievalK = len(hits)
	tr.Coverage = coverage
	seen := map[string]bool{}
	for _, h := range hits {
		if !seen[h.Chunk.PaperID] {
			seen[h.Chunk.PaperID] = true
			tr.HitPapers = append(tr.HitPapers, h.Chunk.PaperID)
		}
	}
	add("retrieve", "work", ms(rStart), 0, fmt.Sprintf("k=%d coverage=%.2f", len(hits), coverage))
	// Whether the planner actually ran. A fallback plan means retrieval used
	// the raw question with no paper filter, so its pointers must be labelled
	// differently.
	routed := understood.plan != "fallback"
	// A refusal can afford a wider pointer search than an answer can afford
	// passages, and without the planner's paper filter it needs one.
	pointerHits := hits
	if !routed && len(hits) > 0 {
		pointerHits = p.retrieveWide(understood.query)
	}
	if tr.DayExhausted {
		tr.Refused = true
		ans := Answer{Text: dailyRefuseText(pointerHits, routed), Refused: true,
			Reason: "daily model budget exhausted", Status: "rate_limited",
			Trace: tr, TotalMs: ms(t0)}
		if streamCB != nil {
			streamCB(ans.Text)
		}
		return ans
	}
	// The planner was shed: the library is local and free, so still return
	// real passage pointers rather than an empty apology.
	if tr.Shed || status == "busy" {
		tr.Refused = true
		ans := Answer{Text: busyRefuseText(pointerHits, routed), Refused: true,
			Reason: "at capacity: shed by admission control", Status: "busy",
			Trace: tr, TotalMs: ms(t0)}
		if streamCB != nil {
			streamCB(ans.Text)
		}
		return ans
	}

	// ---- Step 3: DECIDE (branch) ----------------------------------------
	dStart := time.Now()
	refuse, reason := p.decide(understood, hits, coverage)
	add("decide", "work", ms(dStart), 0, reason)
	if refuse {
		tr.Refused = true
		ans := Answer{
			Text:    refuseText(reason),
			Refused: true, Reason: reason, Trace: tr, Status: status,
			TotalMs: ms(t0),
		}
		if streamCB != nil {
			streamCB(ans.Text)
		}
		return ans
	}

	// ---- Step 4: DRAFT (model call, streamed) ---------------------------
	// The check->draft loop only helps if the input actually changes. On a
	// failed check we widen retrieval (more pieces, cross-paper) so the second
	// draft has new evidence; re-asking with identical passages cannot converge.
	var ttft float64
	revise := 0
	var draft string
	curHits := hits
	feedback := ""
	for {
		dStart, dWait := time.Now(), tr.rawWaitMs()
		firstTok := true
		cb := func(tok string) {
			if firstTok {
				ttft = ms(t0)
				firstTok = false
			}
			if streamCB != nil {
				streamCB(tok)
			}
		}
		var dStatus string
		var draftTTFTRel float64
		beforeIn, beforeOut = tr.TokensIn, tr.TokensOut
		draft, dStatus, draftTTFTRel = p.draft(ctx, q, curHits, feedback, prov, cb, &tr)
		lastIn, lastOut = tr.TokensIn-beforeIn, tr.TokensOut-beforeOut
		if ttft == 0 && draftTTFTRel > 0 {
			// Request-level TTFT = time spent before the draft call plus the
			// provider's internal time to first token.
			ttft = ms(dStart) + draftTTFTRel
		}
		add("draft", "work", ms(dStart)-(tr.rawWaitMs()-dWait), revise, fmt.Sprintf("chars=%d", len(draft)))
		usageStep("draft")

		// Admission control shed this draft: return the capacity message (with
		// local pointers), not a generic model failure.
		if dStatus == "busy" {
			status = "busy"
			tr.Refused = true
			draft = busyRefuseText(curHits, true)
			break
		}

		// An honest refusal needs no verification and no revision: there is
		// nothing to support. Re-drafting it just burns the token budget.
		if strings.Contains(draft, "NOT_IN_LIBRARY") {
			tr.Refused = true
			break
		}
		if dStatus != "ok" || strings.TrimSpace(draft) == "" {
			// Never pass an empty or unavailable draft off as an answer. Daily
			// exhaustion has a specific immediate message; other failures get
			// the general honest fallback.
			status = dStatus
			if status == "ok" {
				status = "error"
			}
			tr.Refused = true
			if tr.DayExhausted {
				draft = dailyRefuseText(curHits, true)
			} else {
				draft = refuseText("model unavailable (" + status + ")")
			}
			break
		}

		// ---- Step 5: CHECK (model call) -> bounded loop ---------------
		cStart, cWait := time.Now(), tr.rawWaitMs()
		beforeIn, beforeOut = tr.TokensIn, tr.TokensOut
		ok, problem := p.check(ctx, q, draft, curHits, prov, &tr)
		lastIn, lastOut = tr.TokensIn-beforeIn, tr.TokensOut-beforeOut
		add("check", "work", ms(cStart)-(tr.rawWaitMs()-cWait), revise, problem)
		usageStep("check")
		// If the daily budget vanished mid-answer (it can happen between the
		// draft and the check), stop immediately with the daily message rather
		// than spending a revision on a call that cannot succeed.
		if tr.DayExhausted {
			status = "rate_limited"
			tr.Refused = true
			draft = dailyRefuseText(curHits, true)
			break
		}
		// The checker was shed. There is a draft but no verdict, and an
		// unverified answer must not be sent; return the capacity message.
		if tr.Shed {
			status = "busy"
			tr.Refused = true
			draft = busyRefuseText(curHits, true)
			break
		}
		if ok {
			break
		}
		if revise >= p.Cfg.MaxRevisions {
			// Checking is a safety gate, not an advisory annotation. If the
			// answer remains unsupported or the checker is unavailable after
			// the bounded retry, refuse rather than fail open.
			tr.Refused = true
			status = "error"
			draft = refuseText("could not verify the answer: " + problem)
			break
		}
		revise++
		feedback = problem
		// Search again with the original question and no paper filter. Keep
		// the original evidence and replace duplicate/weaker passages, while
		// respecting the same prompt-size cap.
		wider, _ := p.retrieve(q, "any")
		curHits = mergeRevisionHits(curHits, wider, p.Cfg.Settings.Pieces)
	}

	// ---- citations -------------------------------------------------------
	cites := extractCitations(draft, curHits)
	// Which paper may the answer cite? By default the planner's choice, which
	// is what keeps the cross-paper trap closed. With ScopeFromQuestion the
	// question's own words decide, because a wrong planner guess must not turn
	// a correctly cited answer into a refusal (q20).
	scope := understood.paper
	if p.Cfg.ScopeFromQuestion {
		scope = questionScope(q)
	}
	if !tr.Refused && !validCitationSet(draft, curHits, scope) {
		// A syntactically plausible citation is not enough: it must point to a
		// supplied passage and respect the planned paper scope. This catches
		// fabricated pages and the real Moshi-for-Transformer failure mode.
		tr.Refused = true
		status = "error"
		draft = refuseText("answer citations did not match the retrieved evidence")
		cites = nil
	}
	ans := Answer{
		Text: draft, Citations: cites, Trace: tr, Status: status,
		Refused: strings.Contains(draft, "NOT_IN_LIBRARY") || tr.Refused,
		TTFTMs:  ttft, TotalMs: ms(t0),
	}
	return ans
}

// understood is the result of step 1.
type understood struct {
	query  string
	intent string
	plan   string
	paper  string
}

var jsonBlock = regexp.MustCompile(`(?s)\{.*\}`)

// understandSchema constrains the planner output so no JSON parsing is needed.
var understandSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query":  map[string]any{"type": "string"},
		"paper":  map[string]any{"type": "string", "enum": []string{"attention", "moshi", "chunking", "any"}},
		"intent": map[string]any{"type": "string", "enum": []string{"factual", "comparison", "howto", "other"}},
	},
	"required":             []string{"query", "paper", "intent"},
	"additionalProperties": false,
}

// checkSchema constrains the verifier output.
var checkSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"ok":      map[string]any{"type": "boolean"},
		"problem": map[string]any{"type": "string"},
	},
	"required":             []string{"ok", "problem"},
	"additionalProperties": false,
}

func (p *Pipeline) understand(ctx context.Context, q string, prov provider.Provider, tr *Trace) (understood, string) {
	// Deliberately terse, and constrained by a schema: this step is on the hot
	// path of every request and on the free tier tokens, not requests, are the
	// binding limit.
	sys := `Turn the question into a short keyword search query over three papers (attention, moshi, chunking).`
	req := provider.Request{
		Model: p.Cfg.RouterModel,
		Messages: []provider.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: q},
		},
		MaxTokens: p.Cfg.RouterMaxTokens, Temperature: 0,
		JSONSchema: understandSchema,
	}
	res, status := p.callWithRetry(ctx, prov, req, tr, "understand")
	out := understood{query: q, intent: "other", paper: "any", plan: "fallback"}
	if status == "ok" {
		if m := jsonBlock.FindString(res.Text); m != "" {
			var v struct {
				Query  string `json:"query"`
				Paper  string `json:"paper"`
				Intent string `json:"intent"`
			}
			if json.Unmarshal([]byte(m), &v) == nil {
				if strings.TrimSpace(v.Query) != "" {
					out.query = v.Query
				}
				if v.Paper != "" {
					out.paper = v.Paper
				}
				if v.Intent != "" {
					out.intent = v.Intent
				}
				out.plan = "planned:" + v.Intent + "/" + v.Paper
			}
		}
	}
	return out, status
}

// retrieveWide is the pointer search for a refusal that could not be
// paper-routed. The answer path keeps only k passages because every one is
// paid for in prompt tokens; a refusal spends nothing, so search wider. With
// k=6 the correct paper can be absent entirely from a mixed query, which is
// exactly the wrong moment to show an empty shelf.
func (p *Pipeline) retrieveWide(query string) []retrieval.Scored {
	cands := p.Index.Search(query, 40)
	if p.Cfg.Settings.Rerank {
		cands = retrieval.RerankWithHint(query, "any", cands)
	}
	return cands
}

// Retrieve exposes the retrieval step to the offline quality measurement: the
// same search the answer path uses, with no model call and no admission. It
// exists so retrieval settings can be compared across every question without
// spending the daily token budget.
func (p *Pipeline) Retrieve(query, paperHint string) []retrieval.Scored {
	hits, _ := p.retrieve(query, paperHint)
	return hits
}

func (p *Pipeline) retrieve(query, paperHint string) ([]retrieval.Scored, float64) {
	cands := p.Index.Search(query, maxInt(p.Cfg.Settings.Pieces*3, 12))
	// If the planner committed to a single paper, respect it: the words are
	// shared across this library ("Transformer" appears in all three papers, in
	// the chunking paper only as a baseline), so an unfiltered search mixes
	// papers that answer different questions.
	if paperHint != "" && paperHint != "any" {
		filtered := cands[:0:0]
		for _, c := range cands {
			if c.Chunk.PaperID == paperHint {
				filtered = append(filtered, c)
			}
		}
		// Only apply the filter if it leaves something to work with.
		if len(filtered) >= minInt(3, p.Cfg.Settings.Pieces) {
			cands = filtered
		}
	}
	if p.Cfg.Settings.Rerank {
		cands = retrieval.RerankWithHint(query, paperHint, cands)
	}
	if len(cands) > p.Cfg.Settings.Pieces {
		cands = cands[:p.Cfg.Settings.Pieces]
	}
	// coverage = fraction of query terms found in the retrieved set.
	coverage := coverageOf(query, cands)
	return cands, coverage
}

func (p *Pipeline) decide(u understood, hits []retrieval.Scored, coverage float64) (bool, string) {
	if len(hits) == 0 {
		return true, "no passages retrieved"
	}
	if p.Cfg.RefuseOnWeak && coverage < p.Cfg.MinCoverage {
		return true, fmt.Sprintf("weak retrieval coverage %.2f < %.2f", coverage, p.Cfg.MinCoverage)
	}
	return false, fmt.Sprintf("answer with %d passages (coverage %.2f)", len(hits), coverage)
}

func (p *Pipeline) draft(ctx context.Context, q string, hits []retrieval.Scored, feedback string, prov provider.Provider, cb func(string), tr *Trace) (string, string, float64) {
	sys := `You answer questions about exactly three research papers, using ONLY the passages provided.
Rules:
- Every factual claim must carry a citation of the form [paper p.N] where paper is attention, moshi, or chunking and N is the page number.
- If the passages do not contain the answer, reply exactly: NOT_IN_LIBRARY
- Never guess. Never use outside knowledge. Be concise.`
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for i, h := range hits {
		// Deliberately NOT "[%d]": a bare bracketed number in the same shape as
		// the required citation made the model emit things like "【3 p.2】"
		// (passage index + page), which is not a valid citation and loses the
		// paper. "Passage N:" keeps the two roles distinct.
		sb.WriteString(fmt.Sprintf("\nPassage %d — %s [%s p.%d]\n%s\n", i+1, h.Chunk.Title, h.Chunk.PaperID, h.Chunk.Page, h.Chunk.Text))
	}
	sb.WriteString("\nQuestion: " + q)
	if strings.TrimSpace(feedback) != "" {
		sb.WriteString("\nThe previous answer failed verification: " + truncate(sanitize(feedback), 200))
		sb.WriteString("\nCorrect that failure using only these passages. Do not repeat unsupported claims.")
	}
	req := provider.Request{
		Model: p.Cfg.Model,
		Messages: []provider.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: sb.String()},
		},
		MaxTokens: p.Cfg.MaxTokens, Temperature: p.Cfg.Temperature,
	}
	// The provider streams and measures its own TTFT relative to the moment
	// it sent the HTTP request. The caller adds the draft call's offset
	// inside the whole request to get the request-level time to first token.
	req.OnToken = func(tok string) {
		if cb != nil {
			cb(tok)
		}
	}
	res, status := p.callWithRetry(ctx, prov, req, tr, "draft")
	if status != "ok" {
		return "", status, 0
	}
	if req.OnToken == nil && cb != nil {
		for _, tok := range splitTokens(res.Text) {
			cb(tok)
		}
	}
	return res.Text, status, res.TTFTMs
}

func (p *Pipeline) check(ctx context.Context, q, draft string, hits []retrieval.Scored, prov provider.Provider, tr *Trace) (bool, string) {
	if strings.Contains(draft, "NOT_IN_LIBRARY") {
		return true, "refused by model"
	}
	sys, user := checkPrompt(draft, hits)
	if p.Cfg.LenientCheck {
		sys, user = checkPromptLenient(draft, hits)
	}
	req := provider.Request{
		Model: p.Cfg.CheckModel,
		Messages: []provider.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		MaxTokens: p.Cfg.CheckMaxTokens, Temperature: 0,
		JSONSchema: checkSchema,
	}
	res, status := p.callWithRetry(ctx, prov, req, tr, "check")
	if status != "ok" {
		return false, "checker unavailable: " + status
	}
	ok, problem, parsed := parseCheck(res.Text)
	if !parsed {
		return false, "checker returned an unparseable verdict"
	}
	return ok, problem
}

// maxCheckPassageChars bounds one passage in the check prompt. It is a guard
// against a pathological chunk, not a summary: real chunks are 1,300–2,700
// characters, so the checker normally reads every word the drafter read.
const maxCheckPassageChars = 4000

// checkPrompt builds the verification call.
//
// The checker must see the same passages the drafter saw. It used to receive
// the first 200 characters of each one, which made the check blind to any fact
// that sat deeper in the chunk: on the real sweep, q02's "h = 8 … dk = dv = 64"
// sits at character ~900 of the attention p.5 chunk, so a correct, cited
// answer was judged unsupported and the pipeline refused it. A false refusal
// is not a safe failure here — it is a wrong answer, and it took a full
// re-measurement to find it.
func checkPrompt(draft string, hits []retrieval.Scored) (string, string) {
	sys := `Check the answer against the passages. Each passage is quoted in full. ` +
		`Return ok=false only if a claim is unsupported or a claim has no citation.`
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for _, h := range hits {
		sb.WriteString(fmt.Sprintf("- [%s p.%d] %s\n", h.Chunk.PaperID, h.Chunk.Page,
			truncate(h.Chunk.Text, maxCheckPassageChars)))
	}
	sb.WriteString("\nAnswer:\n" + draft)
	return sys, sb.String()
}

// checkPromptLenient is the repaired verifier. The strict prompt asks for a
// citation per sentence, and the drafter writes one supporting citation plus
// connective prose ("The authors trained ... and used ...", "i.e. ..."), so a
// correct answer with the fact properly cited was rejected for the sentences
// that carry no new fact. q07, q08 and q20 were lost that way. The gate still
// refuses an uncited or contradicted claim; it no longer demands a citation on
// every sentence.
func checkPromptLenient(draft string, hits []retrieval.Scored) (string, string) {
	sys := `Check the answer against the passages. Each passage is quoted in full. ` +
		`Return ok=false only if a factual claim is contradicted by the passages, or a factual claim carries no citation at all. ` +
		`Connective prose, definitions restated from a cited passage and restatements of the question are not separate claims.`
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for _, h := range hits {
		sb.WriteString(fmt.Sprintf("- [%s p.%d] %s\n", h.Chunk.PaperID, h.Chunk.Page,
			truncate(h.Chunk.Text, maxCheckPassageChars)))
	}
	sb.WriteString("\nAnswer:\n" + draft)
	return sys, sb.String()
}

// questionScope returns the paper a question names, or "any" when it names
// none or more than one. It is deliberately literal: only an explicit paper
// name counts, so a question about Moshi cannot be answered from the
// Transformer paper even if the planner guessed that way.
func questionScope(q string) string {
	low := strings.ToLower(q)
	type named struct {
		id  string
		res []string
	}
	namedPapers := []named{
		{"attention", []string{"attention is all you need", "attention paper", "attention is all"}},
		{"moshi", []string{"moshi"}},
		{"chunking", []string{"dynamic chunking", "chunking paper", "chunking method", "h-net", "hnet"}},
	}
	found := ""
	for _, p := range namedPapers {
		for _, needle := range p.res {
			if strings.Contains(low, needle) {
				if found != "" && found != p.id {
					return "any"
				}
				found = p.id
				break
			}
		}
	}
	if found == "" {
		return "any"
	}
	return found
}

// sanitize collapses whitespace so trace lines stay single-line and readable.
func sanitize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// parseCheck extracts {ok, problem} from the checker reply, tolerating the
// truncated JSON that a reasoning model can emit when it runs out of tokens.
func parseCheck(text string) (bool, string, bool) {
	if m := jsonBlock.FindString(text); m != "" {
		var v struct {
			OK      bool   `json:"ok"`
			Problem string `json:"problem"`
		}
		if json.Unmarshal([]byte(m), &v) == nil {
			if v.OK {
				return true, "ok", true
			}
			return false, "unsupported: " + truncate(v.Problem, 80), true
		}
		// Truncated JSON: recover the "ok" value directly.
		low := strings.ToLower(m)
		if i := strings.Index(low, `"ok"`); i >= 0 {
			rest := low[i:]
			if strings.Contains(rest, "true") {
				return true, "ok", true
			}
			if strings.Contains(rest, "false") {
				return false, "unsupported (truncated check)", true
			}
		}
	}
	low := strings.ToLower(text)
	if strings.Contains(low, `"ok": true`) || strings.Contains(low, `"ok":true`) {
		return true, "ok", true
	}
	if strings.Contains(low, `"ok": false`) || strings.Contains(low, `"ok":false`) {
		return false, "unsupported (truncated check)", true
	}
	return true, "", false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// citeRe matches the required citation form, e.g. "[attention p.5]". The
// model occasionally emits CJK fullwidth brackets 【...】 instead of ASCII ones
// (a tokenizer artifact), and omits the "p." or the space; all of those still
// point at a real passage, so accept them rather than silently dropping the
// citation and grading the answer as uncited.
var citeRe = regexp.MustCompile(`[\[【](attention|moshi|chunking)\s*,?\s*p\.?\s*(\d+)[\]】]`)

func extractCitations(text string, hits []retrieval.Scored) []Citation {
	byPaperPage := map[string]retrieval.Scored{}
	for _, h := range hits {
		byPaperPage[fmt.Sprintf("%s:%d", h.Chunk.PaperID, h.Chunk.Page)] = h
	}
	var out []Citation
	seen := map[string]bool{}
	for _, m := range citeRe.FindAllStringSubmatch(text, -1) {
		key := m[1] + ":" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		pg := atoi(m[2])
		c := Citation{PaperID: m[1], Page: pg, ChunkID: key}
		if h, ok := byPaperPage[key]; ok {
			c.Section = h.Chunk.Section
			c.ChunkID = h.Chunk.ID
		}
		out = append(out, c)
	}
	return out
}

func validCitationSet(text string, hits []retrieval.Scored, paperScope string) bool {
	cites := extractCitations(text, nil)
	if len(cites) == 0 {
		return false
	}
	available := map[string]bool{}
	for _, h := range hits {
		available[fmt.Sprintf("%s:%d", h.Chunk.PaperID, h.Chunk.Page)] = true
	}
	for _, c := range cites {
		if paperScope != "" && paperScope != "any" && c.PaperID != paperScope {
			return false
		}
		if !available[fmt.Sprintf("%s:%d", c.PaperID, c.Page)] {
			return false
		}
	}
	return true
}

// mergeRevisionHits introduces new evidence without allowing a revision loop
// to grow the prompt beyond the configured retrieval budget.
func mergeRevisionHits(original, wider []retrieval.Scored, limit int) []retrieval.Scored {
	out := make([]retrieval.Scored, 0, limit)
	seen := map[string]bool{}
	for _, group := range [][]retrieval.Scored{original, wider} {
		for _, h := range group {
			if len(out) >= limit {
				return out
			}
			if !seen[h.Chunk.ID] {
				seen[h.Chunk.ID] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// refusalPointers lists the pages a refused request can still hand back.
//
// When the planner ran (`routed`), the hits are paper-filtered and reranked,
// so the top few are the same passages a full answer would have used. When the
// planner was itself refused by the limit, retrieval ran on the raw question
// with no paper filter, and the top three can all come from the wrong paper
// ("Transformer" appears in every paper). In that case show the best hit from
// each paper instead, and say so.
func refusalPointers(hits []retrieval.Scored, routed bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range hits {
		if !routed && seen[h.Chunk.PaperID] {
			continue
		}
		seen[h.Chunk.PaperID] = true
		out = append(out, fmt.Sprintf("- %s [%s p.%d]", h.Chunk.Title, h.Chunk.PaperID, h.Chunk.Page))
		if len(out) == 3 {
			break
		}
	}
	return out
}

func busyRefuseText(hits []retrieval.Scored, routed bool) string {
	var sb strings.Builder
	sb.WriteString("I'm at capacity for this minute. I won't start an answer I cannot finish, so I am refusing now rather than making you wait.\n")
	if len(hits) > 0 {
		if routed {
			sb.WriteString("These locally retrieved passages may help in the meantime; they are pointers, not a verified answer:\n")
		} else {
			sb.WriteString("The planning step could not run, so these are the best lexical match from each paper, not a paper-routed answer:\n")
		}
		for _, line := range refusalPointers(hits, routed) {
			sb.WriteString(line + "\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func dailyRefuseText(hits []retrieval.Scored, routed bool) string {
	var sb strings.Builder
	sb.WriteString("Daily model quota reached. I cannot verify a generated answer until the provider's daily quota resets, so I will not guess.\n")
	if len(hits) > 0 {
		if routed {
			sb.WriteString("These locally retrieved passages may help; they are pointers, not a model-generated answer:\n")
		} else {
			sb.WriteString("The planning step could not run, so these are the best lexical match from each paper, not a paper-routed answer:\n")
		}
		for _, line := range refusalPointers(hits, routed) {
			sb.WriteString(line + "\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func coverageOf(query string, hits []retrieval.Scored) float64 {
	terms := retrieval.Tokenize(query)
	if len(terms) == 0 {
		return 0
	}
	uniq := map[string]bool{}
	for _, t := range terms {
		uniq[t] = true
	}
	if len(uniq) == 0 {
		return 0
	}
	var sb strings.Builder
	for _, h := range hits {
		sb.WriteString(" ")
		sb.WriteString(strings.Join(retrieval.Tokenize(h.Chunk.Text), " "))
		sb.WriteString(" ")
	}
	corpus := sb.String()
	found := 0
	for t := range uniq {
		if strings.Contains(corpus, " "+t+" ") {
			found++
		}
	}
	return float64(found) / float64(len(uniq))
}

func refuseText(reason string) string {
	return "I could not find this in the three papers in my library, so I will not guess. " +
		"(reason: " + reason + ")\n\n" +
		"My library contains only: Attention Is All You Need (arXiv 1706.03762v7), " +
		"Moshi (arXiv 2410.00037v1), and Dynamic Chunking for End-to-End Hierarchical " +
		"Sequence Modeling (arXiv 2507.07955v2). If your question refers to one of those, " +
		"try rephrasing with terms from the paper."
}

func addUsage(tr *Trace, u provider.Usage) {
	tr.TokensIn += u.PromptTokens
	tr.TokensOut += u.CompletionTokens
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

// splitTokens splits text into small pieces for token-level streaming.
func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, line := range strings.SplitAfter(s, " ") {
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		out = []string{s}
	}
	return out
}
