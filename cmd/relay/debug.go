package main

import (
	"flag"
	"fmt"
	"strings"

	"relay/internal/papers"
	"relay/internal/retrieval"
)

// cmdDebugRetrieve prints the ranked chunks for a query, so retrieval choices
// can be inspected directly rather than inferred from answers.
//
// -full prints each chunk's whole text, which is what the checker sees; the
// short form truncates to 150 characters, and that difference is the point of
// the flag: a fact that sits at character 900 of a chunk is invisible to
// anything that only reads the first 150.
func cmdDebugRetrieve(args []string) {
	fs := flag.NewFlagSet("debug-retrieve", flag.ExitOnError)
	c := addCommon(fs)
	q := fs.String("q", "", "query")
	paper := fs.String("paper", "any", "paper hint")
	full := fs.Bool("full", false, "print each chunk in full")
	top := fs.Int("k", 6, "how many chunks to print after re-ranking")
	fs.Parse(args)

	lib := c.library()
	set := parseSetting(c.setting)
	lib.ChunkAll(set.PieceWords, set.Overlap)
	ix := retrieval.BuildIndex(lib.Chunks)
	wide := set.Pieces * 3
	if wide < 12 {
		wide = 12
	}
	raw := ix.Search(*q, wide)
	fmt.Printf("== BM25 only (query %q, paper %q, setting %s) ==\n",
		*q, *paper, c.setting)
	for i, s := range raw {
		if i >= *top {
			break
		}
		printChunk(i, s, *full)
	}
	rr := retrieval.RerankWithHint(*q, *paper, raw)
	fmt.Printf("== reranked (top %d) ==\n", *top)
	for i, s := range rr {
		if i >= *top {
			break
		}
		printChunk(i, s, *full)
	}
	_ = papers.Load
}

func printChunk(i int, s retrieval.Scored, full bool) {
	text := s.Chunk.Text
	body := text
	if !full && len(body) > 150 {
		body = body[:150]
	}
	if full {
		fmt.Printf("%2d score=%7.2f %s p.%d words=%d chars=%d\n%s\n",
			i, s.Score, s.Chunk.PaperID, s.Chunk.Page, s.Chunk.Words,
			len(text), indent(replaceNewlines(body)))
		return
	}
	fmt.Printf("%2d score=%7.2f %s p.%d words=%d chars=%d :: %s\n",
		i, s.Score, s.Chunk.PaperID, s.Chunk.Page, s.Chunk.Words,
		len(text), replaceNewlines(body))
}

// indent keeps a full chunk visually separate from the next line.
func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func replaceNewlines(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			out = append(out, ' ')
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}
