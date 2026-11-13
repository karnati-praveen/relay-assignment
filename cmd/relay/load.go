package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"relay/internal/eval"
	"relay/internal/limits"
	"relay/internal/pipeline"
)

// loadScenario describes one load shape.
type loadScenario struct {
	Name        string
	OfferedRate float64       // requests per minute
	Duration    time.Duration // how long to offer
	BurstAt     time.Duration // for burst: how long to stay quiet first
	BurstCount  int           // requests fired at once at BurstAt
	MaxWorkers  int
}

// prebuiltScenarios are the five required load shapes.
func prebuiltScenarios() map[string]loadScenario {
	return map[string]loadScenario{
		// sustainable: offered at the service's real capacity — one request per
		// minute, because 8,000 tokens/minute buys ~1.3 answers at the measured
		// 6,123 tokens each. This is the only shape whose answered requests are
		// not queued, so it is the honest uncontended latency; the others answer
		// far less than they are offered.
		"sustainable": {Name: "sustainable", OfferedRate: 1, Duration: 420 * time.Second, MaxWorkers: 4},
		// steady: 20 offered per minute, ~15x capacity, so the interesting
		// number is how few are answered and what that costs the survivors.
		// Long enough that a few answers survive the 8,000-token minute.
		"steady": {Name: "steady", OfferedRate: 20, Duration: 300 * time.Second, MaxWorkers: 8},
		// burst: quiet, then a sudden spike.
		"burst": {Name: "burst", OfferedRate: 6, Duration: 120 * time.Second, BurstAt: 60 * time.Second, BurstCount: 60, MaxWorkers: 32},
		// sustained overload: far above capacity for the whole window.
		"overload": {Name: "overload", OfferedRate: 180, Duration: 60 * time.Second, MaxWorkers: 48},
		// per-minute limit halved: stand-in RPM=15 instead of 30.
		"rpm_half": {Name: "rpm_half", OfferedRate: 60, Duration: 120 * time.Second, MaxWorkers: 24},
		// daily budget exhausted: both sides' daily token budget is scaled so
		// the boundary is reachable inside the window. One whole-request
		// reservation (6,286) fits in the day and the second does not, so the
		// shape shows one served answer and then the quota reply — instead of a
		// day so small that nothing is ever served.
		"daily_exhausted": {Name: "daily_exhausted", OfferedRate: 40, Duration: 45 * time.Second, MaxWorkers: 16},
	}
}

// cmdLoad runs one load shape and appends rows to results.csv.
func cmdLoad(args []string) {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	c := addCommon(fs)
	scenarioName := fs.String("scenario", "steady", "scenario name")
	out := fs.String("out", "results/results.csv", "output csv for this run")
	durFlag := fs.Duration("duration", 0, "override duration")
	rateFlag := fs.Float64("rate", 0, "override offered rate per minute")
	questions := fs.String("questions", "data/questions.json", "question set to draw from")
	seed := fs.Int64("seed", 7, "rng seed")
	appendMode := fs.Bool("append", false, "append to an existing results file instead of overwriting")
	limitsOn := fs.Bool("limits", false, "enable the pipeline's admission-control bucket")
	tpm := fs.Int("tpm", 8000, "tokens per minute for the bucket")
	rpm := fs.Int("rpm", 30, "requests per minute for the bucket")
	tpd := fs.Int("tpd", 200000, "tokens per day for the bucket")
	detailPath := fs.String("detail", "", "optional JSONL: one row per request with the waiting/working/retrying split")
	fs.Parse(args)

	sc, found := prebuiltScenarios()[*scenarioName]
	if !found {
		log.Fatalf("unknown scenario %q", *scenarioName)
	}
	if *durFlag > 0 {
		sc.Duration = *durFlag
	}
	if *rateFlag > 0 {
		sc.OfferedRate = *rateFlag
	}

	qs := loadQuestions(*questions)
	lib := c.library()
	p := pipeline.New(lib, c.config())
	var bucket *limits.Bucket
	if *limitsOn {
		bucket = limits.New(*tpm, *rpm, *tpd)
		p.SetBucket(bucket)
	}
	prov := c.provider()

	var rows []eval.Row
	var mu sync.Mutex
	var peakQueue int64
	var queue int64

	// The CSV keeps the brief's twelve columns. The optional detail file keeps
	// the trace the brief's section 4 asks for: one request's latency split
	// into waiting (queue for a worker + admission-control window), working
	// (the five pipeline steps) and retrying (backoff inside a step), with the
	// first token timestamped separately from the end of the stream.
	type loadDetail struct {
		RequestID    string                `json:"request_id"`
		Scenario     string                `json:"scenario"`
		Source       string                `json:"source"`
		StartTime    string                `json:"start_time"`
		Status       string                `json:"status"`
		QueuedMs     float64               `json:"queued_ms"`
		FirstTokenMs float64               `json:"first_token_ms"`
		TotalMs      float64               `json:"total_ms"`
		WaitingMs    float64               `json:"waiting_ms"`
		WorkingMs    float64               `json:"working_ms"`
		RetryingMs   float64               `json:"retrying_ms"`
		Retries      int                   `json:"retries"`
		TokensIn     int                   `json:"tokens_in"`
		TokensOut    int                   `json:"tokens_out"`
		Refused      bool                  `json:"refused"`
		Steps        []pipeline.StepTiming `json:"steps,omitempty"`
	}
	var dfile *os.File
	var denc *json.Encoder
	if *detailPath != "" {
		if err := os.MkdirAll(filepath.Dir(*detailPath), 0o755); err != nil {
			log.Fatal(err)
		}
		// Append: each scenario is a separate process and the harness runs five
		// of them into one file. Creating would keep only the last scenario.
		f, err := os.OpenFile(*detailPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		dfile = f
		defer dfile.Close()
		denc = json.NewEncoder(dfile)
	}

	rng := rand.New(rand.NewSource(*seed))
	// Poisson-ish arrival times from the offered rate.
	meanGap := time.Duration(float64(time.Minute) / sc.OfferedRate)

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}

	sem := make(chan struct{}, sc.MaxWorkers)
	var wg sync.WaitGroup
	var reqID int64

	fire := func(q string, idx int64) {
		defer wg.Done()
		cur := atomic.AddInt64(&queue, 1)
		for {
			pk := atomic.LoadInt64(&peakQueue)
			if cur <= pk || atomic.CompareAndSwapInt64(&peakQueue, pk, cur) {
				break
			}
		}
		defer atomic.AddInt64(&queue, -1)

		// The request deadline starts at arrival, not when a worker becomes
		// available. Queueing is part of user-visible latency.
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		var ans pipeline.Answer
		// queued is time spent waiting for a worker slot. It is user-visible
		// latency and the pipeline cannot see it, so it is added to both clocks
		// here. Without this a burst's 30 queued requests would look fast just
		// because the pipeline only starts its stopwatch once a worker frees up.
		queued := 0.0
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			queued = ms(start)
			ans = p.Run(ctx, q, prov, nil)
			ans.TotalMs += queued
			if ans.TTFTMs > 0 {
				ans.TTFTMs += queued
			}
		case <-ctx.Done():
			ans = pipeline.Answer{Status: "timeout", Refused: true, TotalMs: ms(start)}
		}
		st := ans.Status
		if st == "" {
			st = "ok"
		}
		// A shed or rate-limited request never produced model text, so it has
		// no answer to grade: its outcome is the operational `status` column.
		// Grading it "refused_wrongly" would blame the model for the minute's
		// token budget. Only a completed answer is graded.
		grade := "not measured"
		if st == "ok" {
			grade = eval.Grade(qExpect(qs, q), ans.Refused,
				len(ans.Citations) > 0, containsNotInLib(ans.Text))
		}
		row := eval.Row{
			RequestID:        fmt.Sprintf("%s-%d", sc.Name, idx),
			Scenario:         sc.Name,
			Source:           prov.Name(),
			StartTime:        start.UTC().Format(time.RFC3339Nano),
			FirstTokenMs:     ans.TTFTMs,
			TotalMs:          ans.TotalMs,
			Status:           eval.NormalizeStatus(st),
			Retries:          ans.Trace.Retries,
			TokensIn:         ans.Trace.TokensIn,
			TokensOut:        ans.Trace.TokensOut,
			RetrievalSetting: c.setting,
			AnswerGrade:      grade,
		}
		mu.Lock()
		rows = append(rows, row)
		if denc != nil {
			_ = denc.Encode(loadDetail{
				RequestID: row.RequestID, Scenario: row.Scenario, Source: row.Source,
				StartTime: row.StartTime, Status: row.Status, QueuedMs: queued,
				FirstTokenMs: ans.TTFTMs, TotalMs: ans.TotalMs,
				WaitingMs:  queued + ans.Trace.WaitingMs(),
				WorkingMs:  ans.Trace.WorkingMs(),
				RetryingMs: ans.Trace.RetryingMs(),
				Retries:    ans.Trace.Retries,
				TokensIn:   ans.Trace.TokensIn, TokensOut: ans.Trace.TokensOut,
				Refused: ans.Refused, Steps: ans.Trace.Steps,
			})
		}
		mu.Unlock()
	}

	start := time.Now()
	for {
		elapsed := time.Since(start)
		if elapsed > sc.Duration {
			break
		}
		// Burst injection.
		if sc.BurstCount > 0 && elapsed >= sc.BurstAt && elapsed < sc.BurstAt+200*time.Millisecond {
			for i := 0; i < sc.BurstCount; i++ {
				id := atomic.AddInt64(&reqID, 1)
				wg.Add(1)
				go fire(qs.Questions[rng.Intn(len(qs.Questions))].Text, id)
			}
			time.Sleep(sc.BurstAt + 500*time.Millisecond - elapsed)
			continue
		}
		if sc.BurstCount > 0 && elapsed < sc.BurstAt {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		id := atomic.AddInt64(&reqID, 1)
		wg.Add(1)
		go fire(qs.Questions[rng.Intn(len(qs.Questions))].Text, id)

		gap := time.Duration(float64(meanGap) * (0.5 + rng.Float64()))
		time.Sleep(gap)
	}
	offerEnd := time.Now()
	wg.Wait()
	drain := time.Since(offerEnd)

	finalRows := rows
	if *appendMode {
		if existing, err := eval.ReadCSV(*out); err == nil {
			// Drop any earlier rows for this scenario, so re-running a
			// scenario replaces it rather than double-counting it.
			kept := existing[:0:0]
			for _, e := range existing {
				if e.Scenario != sc.Name {
					kept = append(kept, e)
				}
			}
			finalRows = append(kept, rows...)
		}
	}
	if err := eval.WriteCSV(*out, finalRows); err != nil {
		log.Fatal(err)
	}
	// Record the scenario's parameters so Table 2a can report duration and
	// offered rate without hard-coding them in the analysis script.
	writeScenarioMeta(filepath.Join(filepath.Dir(*out), "scenario_meta.json"), sc, prov.Name(), len(rows),
		offerEnd.Sub(start).Seconds(), drain.Seconds(), atomic.LoadInt64(&peakQueue))

	// print a short summary
	var ttft, total []float64
	var ok, rl, busy, failed, retries, tokOut int
	for _, r := range rows {
		switch r.Status {
		case "ok":
			ok++
			// Latency is the latency of an answer; shed rows produced none.
			if r.FirstTokenMs > 0 {
				ttft = append(ttft, r.FirstTokenMs)
			}
			total = append(total, r.TotalMs)
		case "rate_limited":
			rl++
		case "busy":
			busy++
		default:
			failed++
		}
		retries += r.Retries
		tokOut += r.TokensOut
	}
	fmt.Printf("scenario=%s source=%s requests=%d offered=%.1fs drain=%.1fs ok=%d busy=%d rate_limited=%d failed=%d peak_queue=%d\n",
		sc.Name, prov.Name(), len(rows), offerEnd.Sub(start).Seconds(), drain.Seconds(),
		ok, busy, rl, failed, atomic.LoadInt64(&peakQueue))
	if len(rows) > 0 {
		fmt.Printf("  ttft p50=%.0f p95=%.0f p99=%.0f | total p50=%.0f p95=%.0f p99=%.0f | retries=%d tokens_out=%d\n",
			pctl(ttft, 50), pctl(ttft, 95), pctl(ttft, 99),
			pctl(total, 50), pctl(total, 95), pctl(total, 99), retries, tokOut)
	}
}

// writeScenarioMeta merges this scenario's parameters into scenario_meta.json.
// `duration_s` is the offered window (the load shape's definition); `drain_s`
// is the extra time after the last arrival until the last request finished.
// Keeping them separate stops a queue from hiding inside "duration".
func writeScenarioMeta(path string, sc loadScenario, source string, requests int, offeredS, drainS float64, peakQueue int64) {
	meta := map[string]map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &meta)
	}
	meta[sc.Name] = map[string]any{
		"source":       source,
		"requests":     requests,
		"duration_s":   fmt.Sprintf("%.1f", offeredS),
		"drain_s":      fmt.Sprintf("%.1f", drainS),
		"offered_rate": fmt.Sprintf("%.0f", sc.OfferedRate),
		"peak_queue":   peakQueue,
		"burst_at_s":   fmt.Sprintf("%.0f", sc.BurstAt.Seconds()),
		"burst_count":  sc.BurstCount,
		"max_workers":  sc.MaxWorkers,
	}
	b, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(path, b, 0o644)
}

func qExpect(qs eval.QuestionSet, text string) string {
	for _, q := range qs.Questions {
		if q.Text == text {
			return q.Expect
		}
	}
	return "correct"
}

func containsNotInLib(s string) bool {
	const needle = "NOT_IN_LIBRARY"
	if len(s) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func cmdGenConfig(args []string) {
	fs := flag.NewFlagSet("genconfig", flag.ExitOnError)
	out := fs.String("out", "standin.json", "output path")
	fs.Parse(args)
	cfg := map[string]any{
		"queue_mean_ms": 40, "prompt_ms_per_tok": 0.05, "ttft_base_ms": 55,
		"per_tok_ms": 3.0, "latency_sigma_ms": 0.35,
		"slow_tail_prob": 0.05, "slow_tail_extra_ms": 1500,
		"error_prob": 0.01, "concurrency_ref": 2, "slowdown_per_conc": 0.6,
		"rpm": 30, "tpm": 8000, "rpd": 1000, "tpd": 200000,
		"chars_per_token": 4.0, "seed": 42,
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(*out, b, 0o644)
	fmt.Println("wrote", *out)
}
