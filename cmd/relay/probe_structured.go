package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"relay/internal/provider"
)

// cmdProbeStructured isolates the structured-output model call used by the
// pipeline's verification step. It exists so an empty 200 response can be
// diagnosed from evidence instead of inferred from an expensive full run.
func cmdProbeStructured(args []string) {
	fs := flag.NewFlagSet("probe-structured", flag.ExitOnError)
	c := addCommon(fs)
	maxTokens := fs.Int("maxtokens", 300, "completion token budget")
	attempts := fs.Int("attempts", 3, "independent probes")
	noStream := fs.Bool("nostream", false, "use a non-streaming JSON response")
	long := fs.Bool("long", false, "use a checker-sized prompt (six passages plus a draft)")
	out := fs.String("out", "results/structured_probe.jsonl", "jsonl output")
	fs.Parse(args)

	key, err := os.ReadFile(c.keyFile)
	if err != nil {
		log.Fatalf("read key: %v", err)
	}
	prov := provider.NewGroq(strings.TrimSpace(string(key)), c.model)

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ok":      map[string]any{"type": "boolean"},
			"problem": map[string]any{"type": "string"},
		},
		"required":             []string{"ok", "problem"},
		"additionalProperties": false,
	}
	sys := "Check the answer against the passages. Return ok=false only if a claim is unsupported or a claim has no citation."
	user := "Passages:\n- [attention p.7] We used the Adam optimizer with warmup steps 4000.\n\nAnswer:\nAdam with 4,000 warmup steps [attention p.7]."
	if *long {
		var sb strings.Builder
		sb.WriteString("Passages:\n")
		for i := 0; i < 6; i++ {
			sb.WriteString(fmt.Sprintf("- [attention p.%d] %s\n", i+2,
				strings.Repeat("The paper describes attention, optimization and training details in this section. ", 2)))
		}
		sb.WriteString("\nAnswer:\n")
		sb.WriteString(strings.Repeat("The model uses a training schedule and cites the relevant paper passage [attention p.3]. ", 5))
		user = sb.String()
	}
	req := provider.Request{
		Model: c.model, MaxTokens: *maxTokens, Temperature: 0,
		Messages: []provider.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		JSONSchema: schema,
		NoStream:   *noStream,
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	for i := 0; i < *attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res := prov.Chat(ctx, req)
		cancel()
		rec := map[string]any{
			"probe": i, "model": c.model, "max_tokens": *maxTokens,
			"stream": !*noStream, "status": res.Status, "http_status": res.HTTPStatus,
			"content_type": res.ContentType, "stream_events": res.StreamEvents,
			"finish_reason": res.FinishReason, "saw_done": res.SawDone,
			"request_id": res.RequestID, "ttft_ms": res.TTFTMs, "total_ms": res.TotalMs,
			"tokens_in": res.Usage.PromptTokens, "tokens_out": res.Usage.CompletionTokens,
			"reasoning_tokens": res.Usage.ReasoningTokens,
			"text":             res.Text, "err": errStr(res.Err),
			"response_head": res.ResponseHead, "response_tail": res.ResponseTail,
		}
		_ = enc.Encode(rec)
		b, _ := json.MarshalIndent(rec, "", "  ")
		fmt.Println(string(b))
		if res.Status == "rate_limited" && res.RL.RetryAfter != "" {
			var secs float64
			fmt.Sscanf(res.RL.RetryAfter, "%fs", &secs)
			if secs > 0 && secs < 30 {
				time.Sleep(time.Duration(secs * float64(time.Second)))
			}
		}
	}
	fmt.Println("wrote", *out)
}
