// Offline retrieval-quality measurement: for every answerable question, does
// the context this setting retrieves actually contain the sentence that
// answers it? No model call is made, so all settings can be compared without
// spending the daily token budget. The paper hint is the question's own paper
// (or "any"), i.e. routing is held constant so the only variable is retrieval.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"relay/internal/pipeline"
)

const defaultSettings = "p120_k4_rno|p220_k6_ryes|p220_k6_rno|p450_k3_ryes|p900_k2_ryes"

func cmdRecall(args []string) {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	c := addCommon(fs)
	out := fs.String("out", "results/retrieval_recall.csv", "output csv")
	qfile := fs.String("questions", "data/questions.json", "question set with gold patterns")
	settings := fs.String("settings", defaultSettings, "| separated settings")
	fs.Parse(args)

	qs := loadQuestions(*qfile)
	lib := c.library()

	type detail struct {
		Setting string   `json:"setting"`
		QID     string   `json:"qid"`
		Hit     bool     `json:"gold_hit"`
		Missed  []string `json:"patterns_missed,omitempty"`
		Papers  []string `json:"papers"`
		Pages   []int    `json:"pages"`
		Pieces  int      `json:"pieces"`
		Words   int      `json:"context_words"`
	}
	var details []detail

	detailPath := filepath.Join(filepath.Dir(*out), "retrieval_recall.detail.jsonl")
	if err := os.MkdirAll(filepath.Dir(detailPath), 0o755); err != nil {
		log.Fatal(err)
	}
	df, err := os.OpenFile(detailPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer df.Close()
	denc := json.NewEncoder(df)

	type agg struct {
		setting  string
		n, hits  int
		words    int
		pieces   int
		missQIDs []string
	}
	var rows []agg

	for _, s := range strings.Split(*settings, "|") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		cfg := c.config()
		cfg.Settings = parseSetting(s)
		p := pipeline.New(lib, cfg)
		a := agg{setting: s}
		for _, q := range qs.Questions {
			if len(q.Gold) == 0 {
				continue // refusals have no gold statement
			}
			patterns := make([]*regexp.Regexp, 0, len(q.Gold))
			for _, g := range q.Gold {
				re, err := regexp.Compile(g)
				if err != nil {
					log.Fatalf("%s: bad gold pattern %q: %v", q.ID, g, err)
				}
				patterns = append(patterns, re)
			}
			hits := p.Retrieve(q.Text, q.Paper)
			var sb strings.Builder
			d := detail{Setting: s, QID: q.ID, Pieces: len(hits)}
			for _, h := range hits {
				sb.WriteString(h.Chunk.Text)
				sb.WriteString("\n")
				d.Papers = append(d.Papers, h.Chunk.PaperID)
				d.Pages = append(d.Pages, h.Chunk.Page)
				d.Words += h.Chunk.Words
			}
			text := sb.String()
			d.Hit = true
			for i, re := range patterns {
				if !re.MatchString(text) {
					d.Hit = false
					d.Missed = append(d.Missed, q.Gold[i])
				}
			}
			details = append(details, d)
			a.n++
			a.words += d.Words
			a.pieces += d.Pieces
			if d.Hit {
				a.hits++
			} else {
				a.missQIDs = append(a.missQIDs, q.ID)
			}
			if err := denc.Encode(d); err != nil {
				log.Fatalf("write recall detail: %v", err)
			}
		}
		rows = append(rows, a)
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "setting,questions,recall_pct,context_words_mean,pieces_mean")
	for _, a := range rows {
		fmt.Fprintf(f, "%s,%d,%.1f,%.1f,%.2f\n", a.setting, a.n, pct(a.hits, a.n),
			per(a.words, a.n), float64(a.pieces)/float64(a.n))
		fmt.Printf("[%s] recalled=%d/%d (%.1f%%) context=%.0f words%s\n", a.setting, a.hits, a.n,
			pct(a.hits, a.n), per(a.words, a.n), missSuffix(a.missQIDs))
	}
	fmt.Println("wrote", *out, "and", detailPath)
}

func missSuffix(miss []string) string {
	if len(miss) == 0 {
		return ""
	}
	return " missed=" + strings.Join(miss, ",")
}
