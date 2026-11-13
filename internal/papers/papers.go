// Package papers loads the three assigned arXiv papers (at their pinned
// versions) and splits them into citable chunks.
package papers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Paper is one assigned paper.
type Paper struct {
	ID       string // short key: attention, moshi, chunking
	Title    string
	ArxivID  string
	Version  string
	Filename string
	Pages    []string // raw text per page
}

// Chunk is a retrievable passage with enough provenance to cite.
type Chunk struct {
	ID      string
	PaperID string
	Title   string
	Page    int
	Section string
	Text    string
	Words   int
}

// Library holds all papers and their chunks.
type Library struct {
	Papers []*Paper
	Chunks []Chunk
}

var pageMarker = regexp.MustCompile(`(?m)^<<<PAGE (\d+)>>>\s*$`)

// Load reads the extracted text files from dir and builds papers.
func Load(dir string) (*Library, error) {
	meta := []struct {
		id, title, arxiv, version, file string
	}{
		{"attention", "Attention Is All You Need", "1706.03762", "v7", "attention_1706.03762v7.txt"},
		{"moshi", "Moshi: a speech-text foundation model for real-time dialogue", "2410.00037", "v1", "moshi_2410.00037v1.txt"},
		{"chunking", "Dynamic Chunking for End-to-End Hierarchical Sequence Modeling", "2507.07955", "v2", "chunking_2507.07955v2.txt"},
	}
	lib := &Library{}
	for _, m := range meta {
		raw, err := os.ReadFile(filepath.Join(dir, m.file))
		if err != nil {
			return nil, err
		}
		p := &Paper{
			ID: m.id, Title: m.title, ArxivID: m.arxiv, Version: m.version,
			Filename: m.file,
		}
		// Split on <<<PAGE n>>> markers.
		matches := pageMarker.FindAllStringSubmatchIndex(string(raw), -1)
		if len(matches) == 0 {
			p.Pages = []string{string(raw)}
		} else {
			for i, mt := range matches {
				start := mt[1]
				if i+1 < len(matches) {
					p.Pages = append(p.Pages, string(raw[start:matches[i+1][0]]))
				} else {
					p.Pages = append(p.Pages, string(raw[start:]))
				}
			}
		}
		lib.Papers = append(lib.Papers, p)
	}
	return lib, nil
}

// ChunkAll chunks every paper with the given piece size (words) and overlap.
func (l *Library) ChunkAll(pieceWords, overlapWords int) {
	l.Chunks = nil
	for _, p := range l.Papers {
		l.Chunks = append(l.Chunks, chunkPaper(p, pieceWords, overlapWords)...)
	}
}

// chunkPaper splits one paper into overlapping chunks of roughly pieceWords.
func chunkPaper(p *Paper, pieceWords, overlap int) []Chunk {
	var out []Chunk
	// Build a flat list of (page, paragraph). Long paragraphs are split on
	// sentence boundaries first, so no chunk can balloon far past pieceWords.
	type para struct {
		page int
		text string
	}
	var paras []para
	for i, pg := range p.Pages {
		pageNo := i + 1
		for _, block := range strings.Split(pg, "\n\n") {
			block = strings.TrimSpace(block)
			if block == "" {
				continue
			}
			for _, piece := range splitPara(block, pieceWords) {
				paras = append(paras, para{page: pageNo, text: piece})
			}
		}
	}
	// Accumulate paragraphs into chunks.
	var cur []string
	curPage := 0
	curWords := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.Join(cur, "\n\n")
		out = append(out, Chunk{
			ID:      fmt.Sprintf("%s-p%d-%d", p.ID, curPage, len(out)),
			PaperID: p.ID, Title: p.Title, Page: curPage,
			Section: detectSection(text),
			Text:    text, Words: words(text),
		})
		cur = nil
		curWords = 0
	}
	for _, pa := range paras {
		w := words(pa.text)
		if curWords > 0 && curWords+w > pieceWords {
			flush()
		}
		if len(cur) == 0 {
			curPage = pa.page
		}
		cur = append(cur, pa.text)
		curWords += w
		if curWords >= pieceWords {
			flush()
		}
	}
	flush()

	// Overlap pass: if overlapWords > 0, prepend the tail of the previous chunk.
	if overlap > 0 {
		for i := 1; i < len(out); i++ {
			prev := out[i-1].Text
			ps := strings.Fields(prev)
			if len(ps) <= overlap {
				continue
			}
			tail := strings.Join(ps[len(ps)-overlap:], " ")
			if !strings.Contains(out[i].Text, tail) {
				out[i].Text = tail + "\n\n" + out[i].Text
				out[i].Words = words(out[i].Text)
			}
		}
	}
	return out
}

func words(s string) int { return len(strings.Fields(s)) }

var sentenceSplit = regexp.MustCompile(`(?s)([.!?])\s+`)

// splitPara breaks a paragraph into pieces of at most maxWords, cutting on
// sentence boundaries where possible. A single sentence longer than maxWords is
// cut on word boundaries as a last resort.
func splitPara(text string, maxWords int) []string {
	if maxWords <= 0 || words(text) <= maxWords {
		return []string{text}
	}
	// Re-insert sentence-ending punctuation into the split result.
	var sentences []string
	last := 0
	for _, loc := range sentenceSplit.FindAllStringSubmatchIndex(text, -1) {
		end := loc[1] // end of the whitespace after the punctuation
		sentences = append(sentences, strings.TrimSpace(text[last:end]))
		last = end
	}
	if last < len(text) {
		if tail := strings.TrimSpace(text[last:]); tail != "" {
			sentences = append(sentences, tail)
		}
	}
	var out []string
	var cur strings.Builder
	curWords := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			curWords = 0
		}
	}
	for _, s := range sentences {
		w := words(s)
		if w > maxWords {
			// Hard-wrap an oversized sentence on word boundaries.
			flush()
			fields := strings.Fields(s)
			for i := 0; i < len(fields); i += maxWords {
				end := i + maxWords
				if end > len(fields) {
					end = len(fields)
				}
				out = append(out, strings.Join(fields[i:end], " "))
			}
			continue
		}
		if curWords > 0 && curWords+w > maxWords {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString(" ")
		}
		cur.WriteString(s)
		curWords += w
	}
	flush()
	return out
}

var sectionRe = regexp.MustCompile(`(?m)^\s*(\d+(?:\.\d+)*)\s+([A-Z][A-Za-z0-9 ,\-:()]{2,70})\s*$`)

// detectSection returns the last section heading found in a chunk, if any.
func detectSection(text string) string {
	matches := sectionRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return ""
	}
	last := matches[len(matches)-1]
	return strings.TrimSpace(last[1] + " " + last[2])
}
