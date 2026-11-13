// Package provider defines the model-provider abstraction used by the Relay
// pipeline. It has two implementations: GroqProvider (the real hosted service)
// and StandinProvider (a local service we control, used for load the real
// service cannot take).
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Message is one turn in a chat conversation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool describes a function the model may call.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Usage reports token accounting returned by the provider.
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	QueueTimeMs      float64 `json:"queue_time_ms"`
	PromptTimeMs     float64 `json:"prompt_time_ms"`
	CompletionTimeMs float64 `json:"completion_time_ms"`
}

// RateLimitInfo is parsed from the x-ratelimit-* response headers.
type RateLimitInfo struct {
	LimitRequests     int    `json:"limit_requests"`
	RemainingRequests int    `json:"remaining_requests"`
	ResetRequests     string `json:"reset_requests"`
	LimitTokens       int    `json:"limit_tokens"`
	RemainingTokens   int    `json:"remaining_tokens"`
	ResetTokens       string `json:"reset_tokens"`
	RetryAfter        string `json:"retry_after"`
}

// Result is the outcome of one model call.
type Result struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	RL        RateLimitInfo
	TTFTMs    float64 // time to first token, relative to the call start
	TotalMs   float64
	RequestID string
	Status    string // ok, rate_limited, timeout, error
	Attempts  int    // how many provider calls this result took
	Err       error
	Raw       []byte

	// Diagnostics for empty/invalid 200 responses. They are deliberately
	// small, bounded strings so a full evaluation can record why a structured
	// call failed without retaining whole model payloads.
	HTTPStatus   int
	ContentType  string
	StreamEvents int
	FinishReason string
	SawDone      bool
	ResponseHead string
	ResponseTail string
}

// Request is one chat-completion request.
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64
	Tools       []Tool
	ToolChoice  string
	JSONSchema  any // if set, response_format json_schema is requested
	Stream      bool
	// NoStream requests a non-streaming completion. It exists for diagnosing
	// structured-output paths; the answer pipeline always streams.
	NoStream bool

	// OnToken, if set, is called with each streamed token piece.
	OnToken func(string) `json:"-"`
}

// Provider is a model service.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req Request) Result
}

// ---------------------------------------------------------------------------
// Groq (real service)
// ---------------------------------------------------------------------------

// GroqProvider talks to the real Groq OpenAI-compatible endpoint.
type GroqProvider struct {
	APIKey string
	Model  string
	Base   string
	HTTP   *http.Client
}

func NewGroq(apiKey, model string) *GroqProvider {
	return &GroqProvider{
		APIKey: apiKey,
		Model:  model,
		Base:   "https://api.groq.com/openai/v1",
		HTTP:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (g *GroqProvider) Name() string { return "real" }

// StandinProvider talks to our local stand-in service. It speaks the same
// streaming SSE protocol as Groq, so it reuses GroqProvider's client code.
type StandinProvider struct{ GroqProvider }

// NewStandin returns a provider pointed at the local stand-in base URL.
func NewStandin(base string) *StandinProvider {
	return &StandinProvider{GroqProvider{
		APIKey: "",
		Model:  "standin",
		Base:   base,
		HTTP:   &http.Client{Timeout: 300 * time.Second},
	}}
}

func (s *StandinProvider) Name() string { return "stand-in" }

type groqCompletion struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens      int `json:"prompt_tokens"`
		CompletionTokens  int `json:"completion_tokens"`
		TotalTokens       int `json:"total_tokens"`
		CompletionDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type groqStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens      int `json:"prompt_tokens"`
		CompletionTokens  int `json:"completion_tokens"`
		TotalTokens       int `json:"total_tokens"`
		CompletionDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		// Groq timing fields come back at the top level of usage.
		QueueTime      float64 `json:"queue_time"`
		PromptTime     float64 `json:"prompt_time"`
		CompletionTime float64 `json:"completion_time"`
	} `json:"usage"`
	// Groq can return HTTP 200 and then emit an error as an event inside the
	// stream (observed for json_validate_failed when the completion budget is
	// consumed before valid JSON is produced). Without this field the client
	// would see no content, hit EOF, and report an empty success.
	Error *apiError `json:"error"`
}

func (g *GroqProvider) Chat(ctx context.Context, req Request) Result {
	// The provider's model is only a default. Pipeline steps deliberately use
	// different models for planning, drafting and checking; overwriting a
	// non-empty request model here would silently disable that routing.
	if req.Model == "" {
		req.Model = g.Model
	}
	body := buildRequestBody(req, !req.NoStream)
	buf, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", g.Base+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Result{Status: "error", Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if req.NoStream {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("User-Agent", "relay-assignment/1.0")

	start := time.Now()
	resp, err := g.HTTP.Do(httpReq)
	if err != nil {
		status := "error"
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			status = "timeout"
		}
		return Result{Status: status, TotalMs: ms(start), Err: err}
	}
	defer resp.Body.Close()

	rl := parseRateHeaders(resp.Header)
	reqID := resp.Header.Get("x-request-id")
	base := Result{RL: rl, RequestID: reqID, HTTPStatus: resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type")}

	if resp.StatusCode == http.StatusTooManyRequests {
		raw, _ := io.ReadAll(resp.Body)
		base.Status = "rate_limited"
		base.TotalMs = ms(start)
		base.Err = fmt.Errorf("429: %s", strings.TrimSpace(string(raw)))
		base.Raw = raw
		return base
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		base.Status = "error"
		base.TotalMs = ms(start)
		base.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		base.Raw = raw
		return base
	}
	// Non-streaming mode is used only for structured-output diagnosis. Parsing
	// the actual JSON body preserves the distinction between "model returned
	// nothing" and "client failed to read a stream".
	if req.NoStream {
		raw, _ := io.ReadAll(resp.Body)
		base.TotalMs = ms(start)
		base.Raw = raw
		base.ResponseHead = truncateDiag(string(raw), 2048)
		var c groqCompletion
		if err := json.Unmarshal(raw, &c); err != nil {
			base.Status = "error"
			base.Err = fmt.Errorf("HTTP 200 JSON decode: %w: %s", err, truncateDiag(string(raw), 300))
			return base
		}
		if c.Error != nil {
			base.Status = "error"
			base.Err = fmt.Errorf("API error: %s (code=%s type=%s)",
				c.Error.Message, c.Error.Code, c.Error.Type)
			return base
		}
		base.Status = "ok"
		base.RequestID = c.ID
		if len(c.Choices) > 0 {
			base.Text = c.Choices[0].Message.Content
			base.ToolCalls = c.Choices[0].Message.ToolCalls
			base.FinishReason = c.Choices[0].FinishReason
		}
		base.Usage = Usage{
			PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens,
			TotalTokens: c.Usage.TotalTokens, ReasoningTokens: c.Usage.CompletionDetails.ReasoningTokens,
		}
		base.TTFTMs = base.TotalMs
		return base
	}

	// A 200 that is not an event stream cannot be parsed by the streaming
	// client. Surface the actual body rather than returning an empty "ok".
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(resp.Body)
		base.Status = "error"
		base.TotalMs = ms(start)
		base.Err = fmt.Errorf("HTTP 200 with non-SSE content-type %q: %s",
			ct, truncateDiag(strings.TrimSpace(string(raw)), 500))
		base.Raw = raw
		base.ResponseHead = truncateDiag(string(raw), 1024)
		return base
	}

	res := base
	res.Status = "ok"
	headStr, tailStr := "", ""
	appendDiag := func(s string) {
		line := s + "\n"
		if len(headStr) < 1024 {
			headStr += truncateDiag(line, 1024-len(headStr))
		}
		tailStr += line
		if len(tailStr) > 1024 {
			tailStr = tailStr[len(tailStr)-1024:]
		}
	}
	reader := bufio.NewReader(resp.Body)
	var ttftSet bool
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			res.Err = err
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		appendDiag("data: " + data)
		if data == "[DONE]" {
			res.SawDone = true
			break
		}
		var chunk groqStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		res.StreamEvents++
		if chunk.ID != "" {
			res.RequestID = chunk.ID
		}
		if chunk.Error != nil {
			// A 200 response is not a success if the stream carries an error
			// event. Surface the provider's own message so the retry policy and
			// the trace see the real failure instead of an empty answer.
			res.Err = fmt.Errorf("stream error: %s (code=%s type=%s)",
				chunk.Error.Message, chunk.Error.Code, chunk.Error.Type)
			res.Status = "error"
			break
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				res.FinishReason = c.FinishReason
			}
			if c.Delta.Content != "" {
				// Role-only and usage chunks are not answer tokens. Measuring
				// from the first choice would understate the user's real TTFT.
				if !ttftSet {
					res.TTFTMs = ms(start)
					ttftSet = true
				}
				res.Text += c.Delta.Content
				if req.OnToken != nil {
					req.OnToken(c.Delta.Content)
				}
			}
			res.ToolCalls = append(res.ToolCalls, c.Delta.ToolCalls...)
		}
		if chunk.Usage != nil {
			u := chunk.Usage
			res.Usage = Usage{
				PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
				TotalTokens: u.TotalTokens, ReasoningTokens: u.CompletionDetails.ReasoningTokens,
				QueueTimeMs: u.QueueTime * 1000, PromptTimeMs: u.PromptTime * 1000,
				CompletionTimeMs: u.CompletionTime * 1000,
			}
		}
	}
	res.ResponseHead = headStr
	res.ResponseTail = tailStr
	res.TotalMs = ms(start)
	if res.Err == nil && !res.SawDone && res.FinishReason == "" {
		res.Err = fmt.Errorf("stream ended before [DONE] or a finish_reason (events=%d)", res.StreamEvents)
	}
	if res.Err != nil && res.Status == "ok" {
		res.Status = "error"
	}
	if res.TTFTMs == 0 && res.Text != "" {
		res.TTFTMs = res.TotalMs
	}
	return res
}

func truncateDiag(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// request construction
// ---------------------------------------------------------------------------

func buildRequestBody(req Request, stream bool) map[string]any {
	msgs := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		mm := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			mm["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			mm["name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			mm["tool_calls"] = m.ToolCalls
		}
		msgs = append(msgs, mm)
	}
	body := map[string]any{
		"model":       req.Model,
		"messages":    msgs,
		"max_tokens":  req.MaxTokens,
		"temperature": req.Temperature,
		"stream":      stream,
	}
	if stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		body["tools"] = tools
		if req.ToolChoice != "" {
			body["tool_choice"] = req.ToolChoice
		}
	}
	if req.JSONSchema != nil {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "output",
				"strict": true,
				"schema": req.JSONSchema,
			},
		}
	}
	return body
}

func parseRateHeaders(h http.Header) RateLimitInfo {
	atoi := func(s string) int {
		n, _ := strconv.Atoi(strings.TrimSpace(strings.Split(s, ",")[0]))
		return n
	}
	return RateLimitInfo{
		LimitRequests:     atoi(h.Get("x-ratelimit-limit-requests")),
		RemainingRequests: atoi(h.Get("x-ratelimit-remaining-requests")),
		ResetRequests:     h.Get("x-ratelimit-reset-requests"),
		LimitTokens:       atoi(h.Get("x-ratelimit-limit-tokens")),
		RemainingTokens:   atoi(h.Get("x-ratelimit-remaining-tokens")),
		ResetTokens:       h.Get("x-ratelimit-reset-tokens"),
		RetryAfter:        h.Get("retry-after"),
	}
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }
