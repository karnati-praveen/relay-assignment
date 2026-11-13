package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/internal/limits"
	"relay/internal/papers"
	"relay/internal/provider"
	"relay/internal/retrieval"
)

type stubProvider struct {
	responses []provider.Result
	requests  []provider.Request
}

func (s *stubProvider) Name() string { return "stand-in" }
func (s *stubProvider) Chat(_ context.Context, req provider.Request) provider.Result {
	s.requests = append(s.requests, req)
	if len(s.responses) == 0 {
		return provider.Result{Status: "error", Err: errors.New("unexpected call")}
	}
	res := s.responses[0]
	s.responses = s.responses[1:]
	if req.OnToken != nil && res.Text != "" {
		req.OnToken(res.Text)
	}
	return res
}

func testLibrary() *papers.Library {
	return &papers.Library{Papers: []*papers.Paper{{
		ID: "attention", Title: "Attention Is All You Need", ArxivID: "1706.03762", Version: "v7",
		Pages: []string{"Section 5.3 Optimizer. We used the Adam optimizer with warmup steps 4000 and inverse square root decay."},
	}}}
}

func result(status, text string) provider.Result {
	return provider.Result{Status: status, Text: text, TTFTMs: 5}
}

func TestRunPreservesStepModelsAndChecksAnswer(t *testing.T) {
	cfg := DefaultConfig("draft-model")
	cfg.RouterModel = "router-model"
	cfg.CheckModel = "check-model"
	cfg.MaxRetries = 0
	p := New(testLibrary(), cfg)
	prov := &stubProvider{responses: []provider.Result{
		result("ok", `{"query":"Adam optimizer warmup","paper":"attention","intent":"factual"}`),
		result("ok", "Adam with 4,000 warmup steps [attention p.1]."),
		result("ok", `{"ok":true,"problem":""}`),
	}}
	ans := p.Run(context.Background(), "Which optimizer and warmup schedule?", prov, nil)
	if ans.Refused || ans.Status != "ok" || len(ans.Citations) != 1 {
		t.Fatalf("unexpected answer: %+v", ans)
	}
	want := []string{"router-model", "draft-model", "check-model"}
	if len(prov.requests) != len(want) {
		t.Fatalf("got %d calls, want %d", len(prov.requests), len(want))
	}
	for i, model := range want {
		if prov.requests[i].Model != model {
			t.Errorf("call %d model = %q, want %q", i, prov.requests[i].Model, model)
		}
		if len(ans.Trace.Calls) != len(want) || ans.Trace.Calls[i].Model != model {
			t.Errorf("trace does not prove call %d model: %+v", i, ans.Trace.Calls)
		}
	}
}

type badRequestProvider struct{ calls int }

func (b *badRequestProvider) Name() string { return "stand-in" }
func (b *badRequestProvider) Chat(_ context.Context, _ provider.Request) provider.Result {
	b.calls++
	return provider.Result{Status: "error", HTTPStatus: 400, Err: errors.New("HTTP 400: bad request")}
}

func TestRunDoesNotRetryPermanentClientError(t *testing.T) {
	cfg := DefaultConfig("draft")
	cfg.MaxRetries = 3
	p := New(testLibrary(), cfg)
	prov := &badRequestProvider{}
	ans := p.Run(context.Background(), "Which optimizer and warmup schedule?", prov, nil)
	if ans.Trace.Retries != 0 {
		t.Fatalf("a 400 was retried %d time(s)", ans.Trace.Retries)
	}
	for _, c := range ans.Trace.Calls {
		if c.Attempt != 1 {
			t.Fatalf("call %s attempt = %d, want 1 (no retry)", c.Step, c.Attempt)
		}
	}
}

func TestRunEmptyDraftNeverReturnsEmptyAnswer(t *testing.T) {
	cfg := DefaultConfig("draft")
	cfg.MaxRetries = 0
	p := New(testLibrary(), cfg)
	prov := &stubProvider{responses: []provider.Result{
		result("ok", `{"query":"optimizer","paper":"attention","intent":"factual"}`),
		result("ok", ""),
	}}
	ans := p.Run(context.Background(), "Which optimizer?", prov, nil)
	if !ans.Refused || ans.Status != "error" || strings.TrimSpace(ans.Text) == "" {
		t.Fatalf("empty draft was not converted to an explicit refusal: %+v", ans)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("checker ran after an invalid draft: %d calls", len(prov.requests))
	}
}

func TestRunCheckerFailureIsFailClosed(t *testing.T) {
	cfg := DefaultConfig("draft")
	cfg.MaxRetries = 0
	cfg.MaxRevisions = 0
	p := New(testLibrary(), cfg)
	prov := &stubProvider{responses: []provider.Result{
		result("ok", `{"query":"optimizer","paper":"attention","intent":"factual"}`),
		result("ok", "Adam [attention p.1]."),
		result("error", ""),
	}}
	ans := p.Run(context.Background(), "Which optimizer?", prov, nil)
	if !ans.Refused || ans.Status != "error" || strings.Contains(ans.Text, "Adam [attention") {
		t.Fatalf("unchecked answer was allowed through: %+v", ans)
	}
}

func TestRunDailyLimitStopsWithoutRetry(t *testing.T) {
	cfg := DefaultConfig("draft")
	cfg.MaxRetries = 3
	p := New(testLibrary(), cfg)
	prov := &stubProvider{responses: []provider.Result{{
		Status: "rate_limited", Err: errors.New("429 tokens per day (TPD) exhausted"),
	}}}
	ans := p.Run(context.Background(), "Which optimizer?", prov, nil)
	if !ans.Trace.DayExhausted || !ans.Refused || ans.Status != "rate_limited" {
		t.Fatalf("daily exhaustion not surfaced: %+v", ans)
	}
	if len(prov.requests) != 1 || ans.Trace.Retries != 0 {
		t.Fatalf("daily limit was retried: calls=%d retries=%d", len(prov.requests), ans.Trace.Retries)
	}
	if !strings.Contains(ans.Text, "Daily model quota reached") || !strings.Contains(ans.Text, "attention p.1") {
		t.Fatalf("daily response lacks an honest explanation and local pointers: %q", ans.Text)
	}
}

func TestValidCitationSetRejectsWrongPaperAndFabricatedPage(t *testing.T) {
	p := New(testLibrary(), DefaultConfig("draft"))
	hits, _ := p.retrieve("Adam optimizer warmup", "attention")
	if validCitationSet("Moshi says AdamW [moshi p.11].", hits, "attention") {
		t.Fatal("wrong-paper citation accepted")
	}
	if validCitationSet("Unsupported page [attention p.99].", hits, "attention") {
		t.Fatal("citation to unsupplied page accepted")
	}
	if !validCitationSet("Adam [attention p.1].", hits, "attention") {
		t.Fatal("valid citation rejected")
	}
}

func TestRefusalPointersWithoutRoutingShowsOnePerPaper(t *testing.T) {
	hits := []retrieval.Scored{
		{Chunk: papers.Chunk{PaperID: "chunking", Page: 35, Title: "Dynamic Chunking"}},
		{Chunk: papers.Chunk{PaperID: "chunking", Page: 6, Title: "Dynamic Chunking"}},
		{Chunk: papers.Chunk{PaperID: "attention", Page: 9, Title: "Attention"}},
		{Chunk: papers.Chunk{PaperID: "moshi", Page: 13, Title: "Moshi"}},
	}
	// Unrouted: the answer path's k=6 can be three passages from one paper.
	// A refusal must show the best hit from each paper instead.
	got := refusalPointers(hits, false)
	want := []string{"chunking p.35", "attention p.9", "moshi p.13"}
	if len(got) != len(want) {
		t.Fatalf("unrouted pointers = %v, want %v", got, want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Fatalf("pointer %d = %q, want it to contain %q", i, got[i], want[i])
		}
	}
	// Routed: keep the ranking, even when two hits share a paper.
	got = refusalPointers(hits, true)
	if len(got) != 3 || !strings.Contains(got[1], "chunking p.6") {
		t.Fatalf("routed pointers = %v, want the top three in rank order", got)
	}
}

// scriptedProvider answers by which step is calling, so a concurrent test can
// assert that every started request reached its last step.
type scriptedProvider struct {
	mu    sync.Mutex
	calls int
}

func (s *scriptedProvider) Name() string { return "stand-in" }
func (s *scriptedProvider) Chat(_ context.Context, req provider.Request) provider.Result {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if req.Model == "qwen/qwen3.8-27b" {
		return provider.Result{Status: "ok",
			Text:  `{"query":"Adam optimizer warmup","paper":"attention","intent":"fact"}`,
			Usage: provider.Usage{PromptTokens: 60, CompletionTokens: 40}}
	}
	if req.JSONSchema != nil {
		return provider.Result{Status: "ok", Text: `{"ok":true,"problem":""}`,
			Usage: provider.Usage{PromptTokens: 900, CompletionTokens: 20}}
	}
	return provider.Result{Status: "ok", Text: "Adam with warmup [attention p.1].",
		Usage: provider.Usage{PromptTokens: 2400, CompletionTokens: 450}}
}

func TestBurstAdmitsOnlyRequestsItCanFinish(t *testing.T) {
	// A 60-request burst that admits every planner and no draft burns a
	// minute of tokens and answers nothing. One request-level reservation
	// means a request that starts can finish, and a shed one spends nothing.
	p := New(testLibrary(), DefaultConfig("draft"))
	p.SetBucket(limits.New(8000, 30, 0))
	prov := &scriptedProvider{}

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, busy, busyTokens := 0, 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ans := p.Run(context.Background(), "What optimizer did the authors use?", prov, nil)
			mu.Lock()
			defer mu.Unlock()
			switch ans.Status {
			case "ok":
				ok++
			case "busy":
				busy++
				busyTokens += ans.Trace.TokensIn + ans.Trace.TokensOut
			}
		}()
	}
	wg.Wait()

	if ok == 0 {
		t.Fatalf("burst produced no answers (busy=%d)", busy)
	}
	if busyTokens != 0 {
		t.Fatalf("%d shed requests spent %d tokens: work was started that could not finish", busy, busyTokens)
	}
	prov.mu.Lock()
	calls := prov.calls
	prov.mu.Unlock()
	if calls != ok*3 {
		t.Fatalf("provider got %d calls for %d answers, want 3 calls per answer", calls, ok)
	}
	if ok > 2 {
		t.Fatalf("%d answers fit in one 8,000-token minute; reservation is too small", ok)
	}
}

func TestRequestEstimateCoversAMeasuredRequest(t *testing.T) {
	// One reservation must cover what a request actually spends. The measured
	// real answer at p220_k6 was 6,123 tokens: 80 understand, 2,526 draft, and
	// 3,517 check (which now reads whole passages and reasons ~1,450 tokens
	// before its verdict). A reservation below the measured cost parks a
	// request it could have finished.
	cfg := DefaultConfig("draft")
	cfg.Settings = retrieval.Settings{PieceWords: 220, Pieces: 6, Rerank: true}
	p := New(testLibrary(), cfg)
	est := p.requestEstimate()
	if est < 6123 || est > 6700 {
		t.Fatalf("p220_k6 estimate = %d, want at least the measured 6,123 and under 6,700", est)
	}
	// Section 5 of the write-up quotes this number; if the formula moves, the
	// prose must move with it.
	if est != 6286 {
		t.Fatalf("p220_k6 estimate = %d, report.md section 5 says 6,286", est)
	}
	cfg.Settings = retrieval.Settings{PieceWords: 120, Pieces: 4, Rerank: false}
	if smaller := New(testLibrary(), cfg).requestEstimate(); smaller >= est {
		t.Fatalf("p120_k4 estimate %d not smaller than p220_k6 %d", smaller, est)
	}
}

func TestRunShedsInsteadOfHangingWhenWindowIsFull(t *testing.T) {
	cfg := DefaultConfig("draft")
	cfg.MaxQueueWait = 10 * time.Millisecond
	p := New(testLibrary(), cfg)
	b := limits.New(8000, 30, 0)
	p.SetBucket(b)
	// Fill the current minute completely. The next request can only run in
	// the following window, ~a minute away, which is far beyond MaxQueueWait.
	if d := b.Admit(8000); !d.Allow || d.Wait != 0 {
		t.Fatalf("could not fill the window: %+v", d)
	}
	prov := &stubProvider{}
	started := time.Now()
	ans := p.Run(context.Background(), "Which optimizer?", prov, nil)
	elapsed := time.Since(started)
	if ans.Status != "busy" || !ans.Refused || !ans.Trace.Shed {
		t.Fatalf("full window did not shed: %+v", ans)
	}
	if len(prov.requests) != 0 {
		t.Fatalf("shed request still called the provider %d times", len(prov.requests))
	}
	if elapsed > time.Second {
		t.Fatalf("shed took %s; a refusal must be immediate", elapsed)
	}
	if !strings.Contains(ans.Text, "at capacity") || !strings.Contains(ans.Text, "attention p.1") {
		t.Fatalf("busy reply lacks explanation or local pointers: %q", ans.Text)
	}
}
