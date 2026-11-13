// Command relay is the Relay pipeline CLI: it can answer a single question,
// probe the real service's limits, run the graded question set across retrieval
// settings, and run load shapes writing results.csv rows.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"relay/internal/eval"
	"relay/internal/limits"
	"relay/internal/papers"
	"relay/internal/pipeline"
	"relay/internal/provider"
	"relay/internal/retrieval"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "ask":
		cmdAsk(args)
	case "probe":
		cmdProbe(args)
	case "retrieval":
		cmdRetrieval(args)
	case "recall":
		cmdRecall(args)
	case "load":
		cmdLoad(args)
	case "genconfig":
		cmdGenConfig(args)
	case "inspect":
		cmdInspect(args)
	case "debug-retrieve":
		cmdDebugRetrieve(args)
	case "probe-structured":
		cmdProbeStructured(args)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `relay <command> [flags]
  ask        answer a single question through the pipeline
  probe      measure the real service (latency, limits, 429 behaviour)
  retrieval  run the graded question set across retrieval settings (Table 3)
  recall     offline retrieval quality: is the gold sentence in the context?
  load       run a load shape and append rows to results.csv
  probe-structured  diagnose empty structured-output responses
  genconfig  write a default stand-in config`)
}

// ---- shared helpers --------------------------------------------------------

type commonFlags struct {
	model       string
	routerModel string
	checkModel  string
	base        string
	keyFile     string
	paperDir    string
	source      string
	setting     string
	variant     string
	draftMax    int
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.model, "model", "openai/gpt-oss-20b", "model id for the draft call")
	fs.StringVar(&c.routerModel, "router", "qwen/qwen3.8-27b", "model id for understand calls")
	fs.StringVar(&c.checkModel, "checkmodel", "", "model id for the check call (default gpt-oss-20b)")
	fs.StringVar(&c.base, "base", "", "stand-in base URL (sets source=standin)")
	fs.StringVar(&c.keyFile, "keyfile", ".groq_key", "file containing the Groq API key (GROQ_API_KEY is used if the file is absent)")
	fs.StringVar(&c.paperDir, "papers", "papers/text", "directory of extracted paper text")
	fs.StringVar(&c.source, "source", "real", "real or standin")
	fs.StringVar(&c.setting, "setting", "p220_k6_ryes", "retrieval setting label")
	fs.StringVar(&c.variant, "variant", "", "pipeline variant: empty (measured baseline) or fixed (scope-from-question, lenient check)")
	fs.IntVar(&c.draftMax, "draftmaxtokens", 0, "override the draft completion budget (0 = default 700)")
	return c
}

// config builds a pipeline config from the common flags.
func (c *commonFlags) config() pipeline.Config {
	cfg := pipeline.DefaultConfig(c.model)
	cfg.RouterModel = c.routerModel
	if c.checkModel != "" {
		cfg.CheckModel = c.checkModel
	}
	cfg.Settings = parseSetting(c.setting)
	switch c.variant {
	case "", "baseline":
		// Measured configuration; leave every repair switch off.
	case "fixed":
		cfg.ScopeFromQuestion = true
		cfg.LenientCheck = true
	default:
		log.Fatalf("unknown -variant %q (want empty or fixed)", c.variant)
	}
	if c.draftMax > 0 {
		cfg.MaxTokens = c.draftMax
	}
	return cfg
}

func (c *commonFlags) provider() provider.Provider {
	if c.source == "standin" || c.source == "stand-in" || c.base != "" {
		base := c.base
		if base == "" {
			base = "http://127.0.0.1:8099/openai/v1"
		}
		return provider.NewStandin(base)
	}
	// The key comes from the file if it exists, otherwise from the environment,
	// so a reviewer can run one command with no setup beyond GROQ_API_KEY.
	keyText := ""
	if b, err := os.ReadFile(c.keyFile); err == nil {
		keyText = strings.TrimSpace(string(b))
	}
	if keyText == "" {
		keyText = strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	}
	if keyText == "" {
		log.Fatalf("no API key: set GROQ_API_KEY or put it in %s", c.keyFile)
	}
	return provider.NewGroq(keyText, c.model)
}

func (c *commonFlags) library() *papers.Library {
	lib, err := papers.Load(c.paperDir)
	if err != nil {
		log.Fatalf("load papers: %v", err)
	}
	return lib
}

func parseSetting(s string) retrieval.Settings {
	// p220_k6_ryes  or  piece=220,k=6,rerank=yes
	rs := retrieval.Settings{PieceWords: 220, Overlap: 40, Pieces: 6, Rerank: true}
	if s == "" {
		return rs
	}
	if strings.Contains(s, "piece=") {
		var p, k int
		var r string
		fmt.Sscanf(s, "piece=%d,k=%d,rerank=%s", &p, &k, &r)
		return retrieval.Settings{PieceWords: p, Overlap: 40, Pieces: k, Rerank: r == "yes"}
	}
	fmt.Sscanf(s, "p%d_k%d_r%s", &rs.PieceWords, &rs.Pieces, new(string))
	parts := strings.Split(s, "_")
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "p"):
			fmt.Sscanf(strings.TrimPrefix(p, "p"), "%d", &rs.PieceWords)
		case strings.HasPrefix(p, "k"):
			fmt.Sscanf(strings.TrimPrefix(p, "k"), "%d", &rs.Pieces)
		case strings.HasPrefix(p, "r"):
			rs.Rerank = strings.TrimPrefix(p, "r") == "yes"
		}
	}
	rs.Overlap = 40
	return rs
}

// ---- ask -------------------------------------------------------------------

func cmdAsk(args []string) {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	c := addCommon(fs)
	q := fs.String("q", "", "question")
	verbose := fs.Bool("v", false, "verbose trace")
	limitsOn := fs.Bool("limits", true, "enable the pipeline's admission-control bucket (the serving path's default)")
	tpm := fs.Int("tpm", 8000, "tokens per minute for the bucket")
	rpm := fs.Int("rpm", 30, "requests per minute for the bucket")
	tpd := fs.Int("tpd", 200000, "tokens per day for the bucket")
	fs.Parse(args)
	if *q == "" {
		log.Fatal("need -q")
	}
	cfg := c.config()
	p := pipeline.New(c.library(), cfg)
	if *limitsOn {
		p.SetBucket(limits.New(*tpm, *rpm, *tpd))
	}
	prov := c.provider()
	t0 := time.Now()
	ans := p.Run(context.Background(), *q, prov, nil)
	fmt.Println("--- ANSWER ---")
	fmt.Println(ans.Text)
	fmt.Println("--- META ---")
	fmt.Printf("refused=%v ttft_ms=%.1f total_ms=%.1f citations=%d retries=%d tokens_in=%d tokens_out=%d\n",
		ans.Refused, ans.TTFTMs, ms(t0), len(ans.Citations), ans.Trace.Retries, ans.Trace.TokensIn, ans.Trace.TokensOut)
	for _, ct := range ans.Citations {
		fmt.Printf("  cite %s p.%d %s\n", ct.PaperID, ct.Page, ct.Section)
	}
	if *verbose {
		b, _ := json.MarshalIndent(ans.Trace, "", "  ")
		fmt.Println(string(b))
	}
}

// ---- probe -----------------------------------------------------------------

// cmdProbe sends a modest number of real requests and records what the service
// does, including its response headers and 429 behaviour.
func cmdProbe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	c := addCommon(fs)
	n := fs.Int("n", 12, "number of real requests")
	out := fs.String("out", "results/real_probe.jsonl", "output jsonl")
	burst := fs.Int("burst", 0, "if >0, fire this many requests concurrently for the 429 test")
	fs.Parse(args)

	prov := c.provider()
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	lib := c.library()
	lib.ChunkAll(220, 40)
	ix := retrieval.BuildIndex(lib.Chunks)

	// A real request shaped like one pipeline step.
	mk := func(i int) provider.Request {
		return provider.Request{
			Model: c.model,
			Messages: []provider.Message{
				{Role: "system", Content: "Answer in one short sentence."},
				{Role: "user", Content: fmt.Sprintf("Name one component of the Transformer architecture. (probe %d)", i)},
			},
			MaxTokens: 60, Temperature: 0,
		}
	}
	_ = ix

	for i := 0; i < *n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		r := prov.Chat(ctx, mk(i))
		cancel()
		rec := map[string]any{
			"i": i, "status": r.Status, "ttft_ms": r.TTFTMs, "total_ms": r.TotalMs,
			"rl": r.RL, "usage": r.Usage, "request_id": r.RequestID,
			"err": errStr(r.Err),
		}
		enc.Encode(rec)
		fmt.Printf("probe %2d status=%-12s ttft=%7.1f total=%7.1f rpd_remaining=%d tpm_limit=%d tpm_remaining=%d\n",
			i, r.Status, r.TTFTMs, r.TotalMs, r.RL.RemainingRequests, r.RL.LimitTokens, r.RL.RemainingTokens)
		time.Sleep(250 * time.Millisecond)
	}

	// Optional overload: fire many at once to observe the per-minute limit.
	if *burst > 0 {
		fmt.Printf("burst of %d concurrent requests...\n", *burst)
		type res struct{ r provider.Result }
		ch := make(chan res, *burst)
		start := time.Now()
		for i := 0; i < *burst; i++ {
			go func(i int) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				ch <- res{prov.Chat(ctx, mk(1000+i))}
			}(i)
		}
		ok, limited, other := 0, 0, 0
		for i := 0; i < *burst; i++ {
			rr := <-ch
			switch rr.r.Status {
			case "ok":
				ok++
			case "rate_limited":
				limited++
			default:
				other++
			}
			enc.Encode(map[string]any{
				"burst_i": i, "status": rr.r.Status, "total_ms": rr.r.TotalMs,
				"rl": rr.r.RL, "err": errStr(rr.r.Err),
			})
		}
		fmt.Printf("burst done in %.1fs: ok=%d rate_limited=%d other=%d\n", time.Since(start).Seconds(), ok, limited, other)
	}
	fmt.Println("wrote", *out)
}

// ---- retrieval (Table 3) ---------------------------------------------------

func cmdRetrieval(args []string) {
	fs := flag.NewFlagSet("retrieval", flag.ExitOnError)
	c := addCommon(fs)
	out := fs.String("out", "results/retrieval.csv", "output csv")
	qfile := fs.String("questions", "data/questions.json", "question set")
	settings := fs.String("settings", "p120_k4_rno|p220_k6_ryes|p220_k6_rno|p450_k3_ryes|p900_k2_ryes", "| separated settings")
	repeat := fs.Int("repeat", 1, "times to ask each question (for latency)")
	paceMs := fs.Int("pace", 0, "milliseconds to sleep between questions (TPM pacing)")
	budgetWait := fs.Int("budgetwait", 0, "seconds to wait and retry when the daily token budget is exhausted (0 = fail fast)")
	budgetMax := fs.Int("budgetmax", 0, "max budget waits per question (0 = unlimited while budgetwait > 0)")
	tpd := fs.Int("tpd", 200000, "provider's tokens-per-day allowance, used to pace a sweep across its refill")
	resume := fs.Bool("resume", false, "append to the detail file and skip questions it already contains")
	fs.Parse(args)

	prov := c.provider()
	qs := loadQuestions(*qfile)
	lib := c.library()

	type agg struct {
		setting                    string
		n, correct, wrong, refCorr int
		refWrong                   int
		notMeasured                int
		tokIn, tokOut              int
		ttft, total                []float64
	}
	var results []agg
	type detail struct {
		Setting   string   `json:"setting"`
		QID       string   `json:"qid"`
		Rep       int      `json:"rep"`
		Question  string   `json:"question"`
		Expect    string   `json:"expect"`
		Grade     string   `json:"grade_auto"`
		Answer    string   `json:"answer"`
		Citations []string `json:"citations"`
		Pages     []int    `json:"cited_pages"`
		TTFTMs    float64  `json:"ttft_ms"`
		TotalMs   float64  `json:"total_ms"`
		StartTime string   `json:"start_time"`
		Status    string   `json:"status"`
		TokensIn  int      `json:"tokens_in"`
		TokensOut int      `json:"tokens_out"`
		Refused   bool     `json:"refused"`
		// Timing split, so section 4 can cite how much of a slow answer was
		// waiting (limits/backoff) versus working (model + tool).
		WaitingMs  float64 `json:"waiting_ms"`
		WorkingMs  float64 `json:"working_ms"`
		RetryingMs float64 `json:"retrying_ms"`
		// Wall time spent waiting out the provider's daily token budget before
		// this question could run. Kept apart from total_ms: the answer's own
		// latency is the measurement, the wait for budget is the operating cost.
		PacingMs float64              `json:"pacing_ms"`
		Retries  int                  `json:"retries"`
		DayDone  bool                 `json:"day_exhausted"`
		Steps    []string             `json:"steps"`
		Calls    []pipeline.CallTrace `json:"calls"`
		// Variant records which pipeline configuration produced the row, so a
		// post-hoc repair run can never be mistaken for the measured baseline.
		Variant string `json:"variant,omitempty"`
	}
	// The detail file is the durable record: one JSON line per completed
	// question, appended as soon as the answer exists. A sweep that spends
	// hours waiting on a daily token budget must survive a process restart;
	// writing this file only at the end would throw the whole run away.
	disp := "retrieval_" + prov.Name() + ".detail.jsonl"
	if filepath.Base(*out) != "retrieval.csv" {
		disp = strings.TrimSuffix(filepath.Base(*out), filepath.Ext(*out)) + ".detail.jsonl"
	}
	detailPath := filepath.Join(filepath.Dir(*out), disp)
	if err := os.MkdirAll(filepath.Dir(detailPath), 0o755); err != nil {
		log.Fatal(err)
	}
	// A killed process can leave a half-written final line. Drop it before
	// appending, or the next JSON line would be glued to the fragment.
	if *resume {
		if b, err := os.ReadFile(detailPath); err == nil && len(b) > 0 && b[len(b)-1] != '\n' {
			if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
				if err := os.WriteFile(detailPath, b[:i+1], 0o644); err != nil {
					log.Fatal(err)
				}
			} else if err := os.Remove(detailPath); err != nil {
				log.Fatal(err)
			}
		}
	}
	var details []detail
	done := map[string]bool{}
	if *resume {
		if f, err := os.Open(detailPath); err == nil {
			dec := json.NewDecoder(f)
			for {
				var d detail
				if err := dec.Decode(&d); err != nil {
					break
				}
				details = append(details, d)
				done[detailKey(d.Setting, d.QID, d.Rep)] = true
			}
			f.Close()
			fmt.Printf("resume: %d completed question(s) already in %s\n", len(details), detailPath)
		}
	}
	// Seed the pacing from what the last completed question actually spent. The
	// estimate is only a floor: a question that needed a revision can spend twice
	// the reservation, and waiting for the reservation alone would under-pace and
	// earn a 429, which costs a partial attempt on top.
	lastSpent := 0
	for i := len(details) - 1; i >= 0; i-- {
		lastSpent = details[i].TokensIn + details[i].TokensOut
		break
	}
	flags := os.O_CREATE | os.O_WRONLY
	if *resume {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	df, err := os.OpenFile(detailPath, flags, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer df.Close()
	denc := json.NewEncoder(df)

	var settingNames []string

	for _, s := range strings.Split(*settings, "|") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Build a fresh pipeline for this setting so the index matches.
		cfg := c.config()
		cfg.Settings = parseSetting(s)
		p := pipeline.New(lib, cfg)
		settingNames = append(settingNames, s)
		// A per-day allowance is earned back at tpd/86400 tokens a second.
		// Pacing the sweep to that rate is cheaper than discovering it on a 429:
		// a failed attempt still pays for its planner and draft calls, and those
		// tokens are gone whether or not an answer was produced.
		if lastSpent <= 0 {
			lastSpent = p.EstimateRequestTokens()
		}
		for rep := 0; rep < *repeat; rep++ {
			for _, q := range qs.Questions {
				key := detailKey(s, q.ID, rep)
				if done[key] {
					continue
				}
				var ans pipeline.Answer
				var started time.Time
				waits := 0
				questionSpent := 0
				netRetries := 0
				dayWaitMs := 0.0
				if *budgetWait > 0 && lastSpent > 0 {
					if w := budgetWaitSeconds(lastSpent, 0, *tpd); w > 0 {
						fmt.Printf("[%s] %s pacing: waiting %ds to earn back %d tokens\n",
							s, q.ID, w, lastSpent)
						p0 := time.Now()
						time.Sleep(time.Duration(w) * time.Second)
						dayWaitMs += ms(p0)
					}
				}
				for {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					started = time.Now()
					ans = p.Run(ctx, q.Text, prov, nil)
					cancel()
					spent := ans.Trace.TokensIn + ans.Trace.TokensOut
					questionSpent += spent
					// A transient network failure is not a measurement. If no
					// tokens were spent and the transport failed, retry the
					// question rather than let a DNS blip consume its one slot
					// (measured: a 'no such host' lookup cost q01 its place in
					// the first full sweep).
					if spent == 0 && netRetries < 3 &&
						(ans.Status == "timeout" || ans.Status == "error") {
						netRetries++
						fmt.Printf("[%s] %s network failure (%s); retrying in 10s\n",
							s, q.ID, ans.Status)
						time.Sleep(10 * time.Second)
						continue
					}
					// Fail fast by default (matches production). For a controlled
					// measurement run, -budgetwait lets the sweep pause for the
					// provider's own refill window and retry the same question,
					// so a slow daily token bucket cannot masquerade as a wrong
					// answer.
					if *budgetWait <= 0 || !ans.Trace.DayExhausted {
						break
					}
					if *budgetMax > 0 && waits >= *budgetMax {
						break
					}
					// Wait for a whole attempt, not just the call that failed:
					// the retry restarts from the planner, so it re-spends what
					// *this* attempt already burned. Waiting only for retry-after
					// makes the sweep diverge (measured: six waits, 35 minutes,
					// no answer). Only this attempt's spend is earned back; the
					// previous ones were already funded by their own wait.
					waitS := budgetWaitSeconds(spent, dailyRetrySeconds(ans), *tpd)
					if waitS < *budgetWait {
						waitS = *budgetWait
					}
					fmt.Printf("[%s] %s daily budget exhausted; waiting %ds before retry\n", s, q.ID, waitS)
					p0 := time.Now()
					time.Sleep(time.Duration(waitS) * time.Second)
					dayWaitMs += ms(p0)
					waits++
				}
				lastSpent = questionSpent
				hasCite := len(ans.Citations) > 0
				notInLib := strings.Contains(ans.Text, "NOT_IN_LIBRARY")
				g := eval.Grade(q.Expect, ans.Refused, hasCite, notInLib)
				// A request stopped by the daily budget is an operational
				// outcome, not a judgement about the model. It must never be
				// scored as a correct or incorrect answer.
				if ans.Trace.DayExhausted {
					g = "not_measured"
				}
				var cites []string
				var pages []int
				for _, ct := range ans.Citations {
					cites = append(cites, ct.PaperID)
					pages = append(pages, ct.Page)
				}
				var stepNames []string
				for _, st := range ans.Trace.Steps {
					if st.Kind == "wait" {
						stepNames = append(stepNames, fmt.Sprintf("%s(%s)=%.0fms", st.Name, st.Detail, st.Ms))
					}
				}
				d := detail{
					Setting: s, QID: q.ID, Rep: rep, Question: q.Text, Expect: q.Expect,
					Grade: g, Variant: c.variant, Answer: ans.Text, Citations: cites, Pages: pages,
					TTFTMs: ans.TTFTMs, TotalMs: ans.TotalMs,
					StartTime: started.UTC().Format(time.RFC3339Nano), Status: eval.NormalizeStatus(ans.Status),
					TokensIn: ans.Trace.TokensIn, TokensOut: ans.Trace.TokensOut,
					Refused:   ans.Refused,
					WaitingMs: ans.Trace.WaitingMs(), WorkingMs: ans.Trace.WorkingMs(),
					RetryingMs: ans.Trace.RetryingMs(), Retries: ans.Trace.Retries,
					PacingMs: dayWaitMs,
					DayDone:  ans.Trace.DayExhausted, Steps: stepNames, Calls: ans.Trace.Calls,
				}
				details = append(details, d)
				done[key] = true
				// Append and flush before moving on: a restart must not lose an
				// answer that took hours of budget waiting to obtain.
				if err := denc.Encode(d); err != nil {
					log.Fatalf("write detail: %v", err)
				}
				if err := df.Sync(); err != nil {
					log.Fatalf("sync detail: %v", err)
				}
				fmt.Printf("[%s] %s -> %s (%.0fms, %d+%d tok, src=%s)\n", s, q.ID, g, ans.TotalMs, ans.Trace.TokensIn, ans.Trace.TokensOut, prov.Name())
				if *paceMs > 0 {
					time.Sleep(time.Duration(*paceMs) * time.Millisecond)
				}
			}
		}
	}

	// Recompute every aggregate from the durable detail rows, so a resumed run
	// reports the same numbers as an uninterrupted one.
	results = nil
	for _, s := range settingNames {
		a := agg{setting: s}
		for _, d := range details {
			if d.Setting != s {
				continue
			}
			a.n++
			a.tokIn += d.TokensIn
			a.tokOut += d.TokensOut
			// A budget-stopped question produced no answer, so it has no
			// latency to contribute. Keeping its 0 ms in the percentiles would
			// make a sweep that was half stopped look fast; its token cost is
			// still counted, because the sweep really did spend it.
			if d.Grade != "not_measured" {
				a.ttft = append(a.ttft, d.TTFTMs)
				a.total = append(a.total, d.TotalMs)
			}
			switch d.Grade {
			case "correct":
				a.correct++
			case "wrong":
				a.wrong++
			case "refused_correctly":
				a.refCorr++
			case "refused_wrongly":
				a.refWrong++
			case "not_measured":
				a.notMeasured++
			}
		}
		results = append(results, a)
	}

	// write table 3 csv
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, _ := os.Create(*out)
	defer f.Close()
	fmt.Fprintln(f, "setting,questions,correct_pct,wrong_pct,refused_correct_pct,tokens_per_answer,ttft_p50,ttft_p95,total_p50,total_p95")
	for _, a := range results {
		// Percentages are over the genuinely evaluated subset. Budget-stopped
		// questions are reported separately rather than folded into either
		// "correct" or "wrong".
		evaluated := a.n - a.notMeasured
		fmt.Fprintf(f, "%s,%d,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f\n",
			a.setting, evaluated,
			pct(a.correct, evaluated), pct(a.wrong, evaluated), pct(a.refCorr, evaluated),
			// Tokens per actual answer: the sweep's whole spend divided by the
			// questions it actually answered, not by all 20 asked.
			per(a.tokIn+a.tokOut, evaluated),
			pctl(a.ttft, 50), pctl(a.ttft, 95),
			pctl(a.total, 50), pctl(a.total, 95),
		)
		fmt.Printf("[%s] evaluated=%d not_measured=%d correct=%d wrong=%d refused_correct=%d refused_wrong=%d\n",
			a.setting, evaluated, a.notMeasured, a.correct, a.wrong, a.refCorr, a.refWrong)
	}
	fmt.Println("wrote", *out)
}

// detailKey identifies one completed measurement so a resumed sweep can skip
// it without re-spending the token budget.
func detailKey(setting, qid string, rep int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", setting, qid, rep)
}

func loadQuestions(path string) eval.QuestionSet {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read questions: %v", err)
	}
	var qs eval.QuestionSet
	if err := json.Unmarshal(b, &qs); err != nil {
		log.Fatalf("parse questions: %v", err)
	}
	return qs
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// dailyRetrySeconds extracts the provider's own refill estimate from a daily
// 429 body ("Please try again in 9m31.536s"). Returning 0 means no usable hint.
func dailyRetrySeconds(ans pipeline.Answer) int {
	for i := len(ans.Trace.Calls) - 1; i >= 0; i-- {
		msg := ans.Trace.Calls[i].Err
		idx := strings.Index(msg, "try again in ")
		if idx < 0 {
			continue
		}
		rest := msg[idx+len("try again in "):]
		if j := strings.IndexAny(rest, " \n\""); j >= 0 {
			rest = rest[:j]
		}
		rest = strings.TrimRight(rest, ".,;")
		if d, err := time.ParseDuration(rest); err == nil && d > 0 {
			return int(d.Seconds()) + 1
		}
	}
	return 0
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

// dailyRefillPerSec is the rate at which a per-day token allowance is earned
// back. Groq's own 429 arithmetic implies exactly an even spread over the day:
// with 200,000 - 199,437 + 1,886 tokens to fund it answered "try again in
// 9m31.5s", i.e. 1,323 tokens in 571.5 s = 2.31/s (results/real_findings.md).
func dailyRefillPerSec(tpd int) float64 {
	if tpd <= 0 {
		return 0
	}
	return float64(tpd) / 86400.0
}

// budgetWaitSeconds is how long to pause before spending tokens again.
//
// Taking the provider's retry-after alone is not enough, and the gap is not
// cosmetic: retry-after funds the call that failed, while the retry restarts
// the whole request and re-spends the planner and draft tokens already paid
// for. Under that rule each cycle ends shorter than it began, so the sweep
// never accumulates enough for one request (measured: six waits, 35 minutes,
// zero answers). `spent` is therefore added back at the refill rate, so the
// pause buys a *whole* attempt.
func budgetWaitSeconds(spent, retryAfter, tpd int) int {
	if spent < 0 {
		spent = 0
	}
	refill := dailyRefillPerSec(tpd)
	if refill <= 0 {
		return retryAfter
	}
	// +15s of margin: the refill clock is the provider's, not ours.
	return retryAfter + int(float64(spent)/refill) + 15
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func per(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func pctl(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	// Nearest-rank: item ceil(p*N/100), counting from 1, of the sorted values.
	// Same rule as scripts/analyze.py, so every percentile is an observed
	// request and a reviewer can recompute the tables from results.csv.
	k := (p*len(cp) + 99) / 100
	if k < 1 {
		k = 1
	}
	if k > len(cp) {
		k = len(cp)
	}
	return cp[k-1]
}
