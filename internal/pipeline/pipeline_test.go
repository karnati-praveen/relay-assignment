package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"relay/internal/papers"
	"relay/internal/retrieval"
)

// The checker used to read only the first 200 characters of each passage. On
// the real sweep that made it blind to facts that sit deeper in a chunk, so it
// called correct, cited answers unsupported and the pipeline refused them.
// This test pins the fix: the whole passage reaches the checker.
func TestCheckPromptCarriesFullPassage(t *testing.T) {
	fact := "In this work we employ h = 8 parallel attention layers, or heads."
	filler := strings.Repeat("dot products get large, assume the components are independent. ", 14)
	text := filler + fact + " For each of these we use dk = dv = dmodel/h = 64."
	if len(filler) < 200 {
		t.Fatalf("filler too short to reproduce the bug: %d chars", len(filler))
	}
	hits := []retrieval.Scored{{
		Chunk: papers.Chunk{ID: "a5", PaperID: "attention", Page: 5, Text: text},
	}}
	draft := "The base Transformer uses 8 attention heads of dimension 64 [attention p.5]."

	_, user := checkPrompt(draft, hits)
	if !strings.Contains(user, fact) {
		t.Fatalf("check prompt is missing the fact at char %d of the passage: %.300s",
			len(filler), user)
	}
	if !strings.Contains(user, "[attention p.5]") {
		t.Errorf("check prompt lost the passage label")
	}
	if !strings.Contains(user, draft) {
		t.Errorf("check prompt lost the answer")
	}
}

// The model emits the required "[paper p.N]" form most of the time, but
// gpt-oss occasionally emits CJK fullwidth brackets 【...】 or drops the "p.".
// Those citations point at real passages and must not be dropped, or the
// answer gets graded as uncited and the reported quality is wrong. This test
// pins every spelling seen in the real run.
func TestExtractCitations(t *testing.T) {
	cases := []struct {
		text  string
		paper string
		page  int
	}{
		{"The base model uses d_model = 512 [attention p.9].", "attention", 9},
		{"uses 8 heads, 64 each \u3010attention p.5\u3011", "attention", 5},
		{"dropout [attention p8]", "attention", 8},
		{"as in [moshi p. 12]", "moshi", 12},
	}
	for _, c := range cases {
		got := extractCitations(c.text, nil)
		if len(got) != 1 {
			t.Errorf("extractCitations(%q) = %d citations, want 1", c.text, len(got))
			continue
		}
		if got[0].PaperID != c.paper || got[0].Page != c.page {
			t.Errorf("extractCitations(%q) = %s p.%d, want %s p.%d",
				c.text, got[0].PaperID, got[0].Page, c.paper, c.page)
		}
	}

	// Duplicates collapse; an unknown paper is not a citation.
	dup := extractCitations("[attention p.9] and again [attention p.9]", nil)
	if len(dup) != 1 {
		t.Errorf("duplicate citations not collapsed: got %d", len(dup))
	}
	if n := len(extractCitations("see [foo p.3]", nil)); n != 0 {
		t.Errorf("unknown paper accepted: got %d", n)
	}
	// A bare passage index is NOT a citation: mapping "3" to a paper here would
	// let the pipeline credit an answer it cannot actually attribute.
	if n := len(extractCitations("U-Net architecture\u30103 p.2\u3011", nil)); n != 0 {
		t.Errorf("passage index accepted as a paper citation: got %d", n)
	}
}

// The retrieval step must be a real, typed tool call, not an accidental direct
// function call: the arguments arrive as JSON, the schema is checked, and the
// dispatcher rejects everything that is not the declared tool.
func TestInvokeToolIsATypedBoundary(t *testing.T) {
	p := New(testLibrary(), DefaultConfig("draft-model"))
	hits, coverage, err := p.invokeTool("search_library",
		json.RawMessage(`{"query":"Adam optimizer warmup","paper":"attention"}`))
	if err != nil {
		t.Fatalf("search_library: %v", err)
	}
	if len(hits) == 0 || coverage <= 0 {
		t.Fatalf("search_library returned %d hits, coverage %.2f", len(hits), coverage)
	}
	for _, h := range hits {
		if h.Chunk.PaperID != "attention" {
			t.Errorf("paper filter leaked %q", h.Chunk.PaperID)
		}
	}
	for _, c := range []struct{ name, args string }{
		{"delete_library", `{"query":"x"}`},
		{"search_library", `{"query":"   "}`},
		{"search_library", `not json`},
	} {
		if _, _, err := p.invokeTool(c.name, json.RawMessage(c.args)); err == nil {
			t.Errorf("invokeTool(%s, %s) accepted bad input", c.name, c.args)
		}
	}
}

// q20 was refused because the planner guessed the wrong paper and the citation
// guard then rejected a correct [chunking p.1]. Scope from the question keeps
// the guard for the trap shape it was built for while letting a question that
// names no paper be answered from whichever paper actually carries the fact.
func TestQuestionScope(t *testing.T) {
	for _, tc := range []struct{ q, want string }{
		{"Which paper introduces the term 'tokenization' as a barrier to end-to-end foundation models?", "any"},
		{"What were the BLEU scores reported in the Dynamic Chunking paper for WMT 2014 English-to-German?", "chunking"},
		{"What does the Attention Is All You Need paper say about the KV-cache used in Moshi?", "any"},
		{"How many elements does Moshi's text-tokenizer vocabulary contain?", "moshi"},
		{"What architecture does the Dynamic Chunking paper build on?", "chunking"},
		{"What is the RVQ depth used for the audio tokenizer?", "any"},
	} {
		if got := questionScope(tc.q); got != tc.want {
			t.Errorf("questionScope(%q) = %q, want %q", tc.q, got, tc.want)
		}
	}
}

// The repaired verifier must keep refusing what it exists to catch, while not
// demanding a citation for connective prose.
func TestLenientCheckPromptStillRequiresSupport(t *testing.T) {
	hits := []retrieval.Scored{{
		Chunk: papers.Chunk{ID: "c1", PaperID: "chunking", Page: 1,
			Text: "pre-processing steps such as tokenization remain a barrier to true end-to-end foundation models."},
	}}
	sys, user := checkPromptLenient("Tokenization is the barrier [chunking p.1].", hits)
	if !strings.Contains(sys, "contradicted") || !strings.Contains(sys, "no citation") {
		t.Fatalf("lenient prompt lost a refusal condition: %s", sys)
	}
	if !strings.Contains(user, "tokenization remain a barrier") {
		t.Fatalf("lenient prompt is missing the passage: %.200s", user)
	}
	strict, _ := checkPrompt("x", hits)
	if sys == strict {
		t.Fatal("lenient and strict prompts are identical; the switch does nothing")
	}
}
