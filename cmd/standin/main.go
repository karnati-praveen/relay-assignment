// Command standin is a local, controllable stand-in for the Groq chat API.
// It mimics the OpenAI-compatible streaming protocol exactly, so the pipeline
// talks to it unchanged. It can be slow, fail, refuse when a limit is reached,
// slow down under load, and stream its reply one piece at a time.
//
// Everything is calibrated from measurements of the real service; see
// calibration.json / the write-up. Where it is least like the real thing is
// documented in cmd/standin/README.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is the stand-in's behaviour.
type Config struct {
	// Latency model, in milliseconds.
	QueueMeanMs     float64 `json:"queue_mean_ms"`     // server-side queue time
	PromptMsPerTok  float64 `json:"prompt_ms_per_tok"` // prefill cost
	TTFTBaseMs      float64 `json:"ttft_base_ms"`      // fixed time to first token
	PerTokMs        float64 `json:"per_tok_ms"`        // inter-token cost
	LatencySigmaMs  float64 `json:"latency_sigma_ms"`  // lognormal sigma for jitter
	SlowTailProb    float64 `json:"slow_tail_prob"`    // probability of a slow outlier
	SlowTailExtraMs float64 `json:"slow_tail_extra_ms"`

	// Failure model.
	ErrorProb float64 `json:"error_prob"` // probability of a 500
	TimeoutMs float64 `json:"timeout_ms"` // if >0, requests hang this long then drop

	// Load model.
	ConcurrencyRef  int     `json:"concurrency_ref"`   // concurrency at which k=1
	SlowdownPerConc float64 `json:"slowdown_per_conc"` // multiplier added per unit of excess concurrency

	// Rate limits (mirroring Groq free tier).
	RPM int `json:"rpm"`
	TPM int `json:"tpm"`
	RPD int `json:"rpd"`
	TPD int `json:"tpd"`

	CharsPerToken float64 `json:"chars_per_token"`
	Seed          int64   `json:"seed"`
}

func defaultConfig() Config {
	return Config{
		QueueMeanMs:     40,
		PromptMsPerTok:  0.05,
		TTFTBaseMs:      55,
		PerTokMs:        3.0,
		LatencySigmaMs:  0.35,
		SlowTailProb:    0.05,
		SlowTailExtraMs: 1500,
		ErrorProb:       0.01,
		TimeoutMs:       0,
		ConcurrencyRef:  2,
		SlowdownPerConc: 0.6,
		RPM:             30, TPM: 8000, RPD: 1000, TPD: 200000,
		CharsPerToken: 4.0,
		Seed:          42,
	}
}

type limiter struct {
	cfg Config

	mu           sync.Mutex
	windowStart  time.Time
	reqThisMin   int
	tokThisMin   int
	reqToday     int
	tokToday     int
	dayStart     time.Time
	cooloffUntil time.Time
}

func newLimiter(cfg Config) *limiter {
	now := time.Now()
	return &limiter{cfg: cfg, windowStart: now.Truncate(time.Minute), dayStart: now}
}

// admit returns (ok, retryAfterSeconds, reason). A nil-but-ok result admits.
func (l *limiter) admit(estTokens int) (bool, float64, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.windowStart) >= time.Minute {
		l.windowStart = now.Truncate(time.Minute)
		l.reqThisMin = 0
		l.tokThisMin = 0
	}
	if now.Sub(l.dayStart) >= 24*time.Hour {
		l.dayStart = now
		l.reqToday = 0
		l.tokToday = 0
	}
	if l.cfg.RPD > 0 && l.reqToday >= l.cfg.RPD {
		return false, 60, "requests per day"
	}
	if l.cfg.TPD > 0 && l.tokToday+estTokens > l.cfg.TPD {
		return false, 60, "tokens per day"
	}
	if l.cfg.RPM > 0 && l.reqThisMin >= l.cfg.RPM {
		ra := time.Until(l.windowStart.Add(time.Minute)).Seconds()
		return false, ra, "requests per minute"
	}
	if l.cfg.TPM > 0 && l.tokThisMin+estTokens > l.cfg.TPM {
		ra := time.Until(l.windowStart.Add(time.Minute)).Seconds()
		return false, ra, "tokens per minute"
	}
	l.reqThisMin++
	l.reqToday++
	l.tokThisMin += estTokens
	l.tokToday += estTokens
	return true, 0, ""
}

type server struct {
	cfg     Config
	lim     *limiter
	rng     *rand.Rand
	rngMu   sync.Mutex
	conc    int64
	metrics struct {
		total    int64
		limited  int64
		errors   int64
		peakConc int64
	}
}

var activeConc int64

func (s *server) rand() float64 {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return s.rng.Float64()
}

func (s *server) randNorm() float64 {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return s.rng.NormFloat64()
}

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:8099", "listen address")
		cfgPath  = flag.String("config", "", "path to JSON config")
		overload = flag.Float64("slowdown", -1, "override slowdown_per_conc")
		rpm      = flag.Int("rpm", -1, "override rpm")
		rpd      = flag.Int("rpd", -1, "override rpd")
		tpm      = flag.Int("tpm", -1, "override tpm")
		tpd      = flag.Int("tpd", -1, "override tpd")
	)
	flag.Parse()

	cfg := defaultConfig()
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		if err != nil {
			log.Fatalf("read config: %v", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Fatalf("parse config: %v", err)
		}
	}
	if *overload >= 0 {
		cfg.SlowdownPerConc = *overload
	}
	if *rpm >= 0 {
		cfg.RPM = *rpm
	}
	if *rpd >= 0 {
		cfg.RPD = *rpd
	}
	if *tpm >= 0 {
		cfg.TPM = *tpm
	}
	if *tpd >= 0 {
		cfg.TPD = *tpd
	}

	s := &server{cfg: cfg, lim: newLimiter(cfg), rng: rand.New(rand.NewSource(cfg.Seed))}

	mux := http.NewServeMux()
	mux.HandleFunc("/openai/v1/chat/completions", s.chat)
	mux.HandleFunc("/chat/completions", s.chat)
	mux.HandleFunc("/metrics", s.metricsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	log.Printf("stand-in listening on %s (rpm=%d tpm=%d rpd=%d tpd=%d slowdown=%.2f err=%.3f)",
		*addr, cfg.RPM, cfg.TPM, cfg.RPD, cfg.TPD, cfg.SlowdownPerConc, cfg.ErrorProb)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxTokens  int   `json:"max_tokens"`
	Stream     bool  `json:"stream"`
	Tools      []any `json:"tools"`
	ToolChoice any   `json:"tool_choice"`
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	cur := atomic.AddInt64(&activeConc, 1)
	defer atomic.AddInt64(&activeConc, -1)
	if cur > s.metrics.peakConc {
		s.metrics.peakConc = cur
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	atomic.AddInt64(&s.metrics.total, 1)

	// Estimate tokens from characters.
	inChars := 0
	for _, m := range req.Messages {
		inChars += len(m.Content)
	}
	estIn := int(float64(inChars)/s.cfg.CharsPerToken) + 4
	outTokens := req.MaxTokens
	if outTokens <= 0 || outTokens > 400 {
		outTokens = 300
	}
	// Efficiency: model emits fewer tokens than max for a typical answer.
	outTokens = outTokens * 2 / 3
	if outTokens < 20 {
		outTokens = 20
	}

	ok, retryAfter, reason := s.lim.admit(estIn + outTokens)
	if !ok {
		atomic.AddInt64(&s.metrics.limited, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("retry-after", fmt.Sprintf("%d", int(retryAfter)+1))
		w.Header().Set("x-ratelimit-limit-requests", fmt.Sprintf("%d", s.cfg.RPD))
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.Header().Set("x-ratelimit-reset-requests", fmt.Sprintf("%.2fs", retryAfter))
		w.Header().Set("x-ratelimit-limit-tokens", fmt.Sprintf("%d", s.cfg.TPM))
		w.Header().Set("x-ratelimit-remaining-tokens", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("Rate limit reached for %s: %s. Please try again in %ds.", req.Model, reason, int(retryAfter)+1),
				"type":    "rate_limit_error",
				"code":    "rate_limit_exceeded",
			},
		})
		return
	}

	// Random hard failures.
	if s.cfg.ErrorProb > 0 && s.rand() < s.cfg.ErrorProb {
		atomic.AddInt64(&s.metrics.errors, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "internal server error", "type": "server_error"},
		})
		return
	}

	// Load-dependent slowdown: more concurrent requests -> each is slower.
	conc := atomic.LoadInt64(&activeConc)
	excess := float64(conc - int64(s.cfg.ConcurrencyRef))
	if excess < 0 {
		excess = 0
	}
	loadMult := 1.0 + excess*s.cfg.SlowdownPerConc

	// Base latency components.
	jitter := func(base float64) float64 {
		// Lognormal-ish multiplicative jitter.
		f := 1.0 + s.cfg.LatencySigmaMs*s.randNorm()
		if f < 0.2 {
			f = 0.2
		}
		return base * f
	}
	queueMs := jitter(s.cfg.QueueMeanMs) * loadMult
	ttft := jitter(s.cfg.TTFTBaseMs+float64(estIn)*s.cfg.PromptMsPerTok) * loadMult
	if s.cfg.SlowTailProb > 0 && s.rand() < s.cfg.SlowTailProb {
		ttft += s.cfg.SlowTailExtraMs * (0.5 + s.rand())
	}
	perTok := jitter(s.cfg.PerTokMs) * loadMult

	// Decide the reply text.
	reply := synthesize(req)

	// Emit.
	if req.Stream {
		s.streamReply(w, req, reply, queueMs, ttft, perTok, outTokens)
		return
	}
	_ = reply
	time.Sleep(msDuration(queueMs + ttft + perTok*float64(len(reply)/4)))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("standin-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
		"usage": map[string]any{
			"prompt_tokens": estIn, "completion_tokens": outTokens,
			"total_tokens":    estIn + outTokens,
			"queue_time":      queueMs / 1000.0,
			"prompt_time":     float64(estIn) * s.cfg.PromptMsPerTok / 1000.0,
			"completion_time": perTok * float64(outTokens) / 1000.0,
		},
	})
}

// streamReply writes SSE chunks one token piece at a time.
func (s *server) streamReply(w http.ResponseWriter, req chatRequest, reply string, queueMs, ttft, perTok float64, outTokens int) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("x-ratelimit-limit-requests", fmt.Sprintf("%d", s.cfg.RPD))
	w.Header().Set("x-ratelimit-limit-tokens", fmt.Sprintf("%d", s.cfg.TPM))
	w.WriteHeader(http.StatusOK)
	id := fmt.Sprintf("standin-%d", time.Now().UnixNano())

	time.Sleep(msDuration(queueMs))

	emitRaw := func(delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{choice},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}

	// Tool-calling requests: emit an OpenAI-style streamed tool call in one
	// shot after the first-token delay, then finish. This mirrors how Groq
	// streams tool calls (a single delta carrying the full arguments).
	if len(req.Tools) > 0 {
		time.Sleep(msDuration(ttft))
		args := fmt.Sprintf(`{"query": %q, "paper": "any"}`, extractTopic(strings.ToLower(lastUserContent(req))))
		emitRaw(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()),
			"type":     "function",
			"function": map[string]any{"name": "search_library", "arguments": args},
		}}}, "")
		time.Sleep(msDuration(perTok))
		emitRaw(map[string]any{}, "tool_calls")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}

	pieces := strings.SplitAfter(reply, " ")
	if len(pieces) == 0 {
		pieces = []string{""}
	}
	// First token after queue + ttft.
	time.Sleep(msDuration(ttft))
	emit := func(content string) {
		emitRaw(map[string]any{"content": content}, "")
	}
	emit(pieces[0])
	for _, p := range pieces[1:] {
		time.Sleep(msDuration(perTok))
		emit(p)
	}
	// final usage chunk
	inChars := 0
	for _, m := range req.Messages {
		inChars += len(m.Content)
	}
	estIn := int(float64(inChars)/s.cfg.CharsPerToken) + 4
	usage := map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": req.Model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": estIn, "completion_tokens": outTokens, "total_tokens": estIn + outTokens,
			"queue_time": queueMs / 1000.0, "prompt_time": float64(estIn) * s.cfg.PromptMsPerTok / 1000.0,
			"completion_time": perTok * float64(outTokens) / 1000.0,
		},
	}
	b, _ := json.Marshal(usage)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func lastUserContent(req chatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// synthesize produces a plausible reply. It is deterministic in shape, so the
// pipeline's parsing paths exercise the same branches as with the real model.
func synthesize(req chatRequest) string {
	last := strings.ToLower(lastUserContent(req))
	isUnderstand := false
	isCheck := false
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		lc := strings.ToLower(m.Content)
		if strings.Contains(lc, "plan retrieval") || strings.Contains(lc, "short keyword search query") {
			isUnderstand = true
		}
		if strings.Contains(lc, "verify whether") || strings.Contains(lc, "check the answer against the passages") {
			isCheck = true
		}
	}
	switch {
	case isUnderstand:
		return `{"query": "` + extractTopic(last) + `", "paper": "any", "intent": "factual"}`
	case isCheck:
		return `{"ok": true, "problem": ""}`
	default:
		// Return a citation to evidence actually supplied to the draft call.
		// A fixed citation would exercise neither provenance validation nor
		// the citation parser realistically.
		cite := firstSuppliedCitation(last)
		return "Based on the supplied evidence, the library describes the requested method " + cite + "."
	}
}

var suppliedCitation = regexp.MustCompile(`\[(attention|moshi|chunking) p\.[0-9]+\]`)

func firstSuppliedCitation(s string) string {
	if cite := suppliedCitation.FindString(s); cite != "" {
		return cite
	}
	return "[attention p.3]"
}

func extractTopic(s string) string {
	fields := strings.Fields(s)
	if len(fields) > 12 {
		fields = fields[:12]
	}
	return strings.Join(fields, " ")
}

func msDuration(ms float64) time.Duration {
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms * float64(time.Millisecond))
}

func (s *server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total":        atomic.LoadInt64(&s.metrics.total),
		"rate_limited": atomic.LoadInt64(&s.metrics.limited),
		"errors":       atomic.LoadInt64(&s.metrics.errors),
		"peak_conc":    atomic.LoadInt64(&s.metrics.peakConc),
	})
}
