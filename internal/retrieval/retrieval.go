// Package retrieval implements lexical (BM25) retrieval over the paper chunks
// plus an optional deterministic re-ranker, so we can compare quality/cost
// trade-offs across piece size, number of pieces, and re-ranking.
package retrieval

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"relay/internal/papers"
)

// Settings describe one retrieval configuration.
type Settings struct {
	PieceWords int  // chunk size in words
	Overlap    int  // overlap between chunks in words
	Pieces     int  // how many chunks are fetched (k)
	Rerank     bool // whether to apply the re-ranker
}

func (s Settings) String() string {
	r := "no"
	if s.Rerank {
		r = "yes"
	}
	return "piece=" + strconv.Itoa(s.PieceWords) + ",k=" + strconv.Itoa(s.Pieces) + ",rerank=" + r
}

// Scored is a chunk with its retrieval score.
type Scored struct {
	Chunk papers.Chunk
	Score float64
}

// Index is a BM25 index over a set of chunks.
type Index struct {
	Chunks []papers.Chunk
	TF     []map[string]float64 // per chunk term frequencies
	DF     map[string]float64
	DocLen []float64
	AvgDL  float64
	N      float64
}

// tokenRe keeps sub-scripted symbols together, so d_model, d_k and d_ff
// survive tokenization as single distinctive terms. Splitting them into "d"
// and "model" (and then dropping "d" as too short) was a real precision bug:
// it made every occurrence of the word "model" look like a match for d_model.
var tokenRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*(?:_[A-Za-z0-9]+)+|[A-Za-z0-9]+`)

// stopwords are common English words that carry little retrieval signal.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "for": true, "from": true,
	"has": true, "have": true, "in": true, "is": true, "it": true, "its": true,
	"of": true, "on": true, "or": true, "that": true, "the": true, "their": true,
	"this": true, "to": true, "was": true, "were": true, "which": true, "with": true,
	"we": true, "our": true, "they": true, "them": true, "than": true, "then": true,
	"there": true, "these": true, "those": true, "can": true, "could": true,
	"would": true, "should": true, "may": true, "might": true, "will": true,
	"also": true, "such": true, "into": true, "over": true, "about": true,
}

// Tokenize lowercases, splits, removes stopwords and applies light stemming.
func Tokenize(s string) []string {
	raw := tokenRe.FindAllString(strings.ToLower(s), -1)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if len(t) < 2 || stopwords[t] {
			continue
		}
		out = append(out, stem(t))
	}
	return out
}

// stem applies conservative suffix stripping.
func stem(w string) string {
	for _, suf := range []string{"ing", "tion", "ment", "ness", "able", "ible"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return w[:len(w)-len(suf)]
		}
	}
	if len(w) > 3 && strings.HasSuffix(w, "es") {
		return w[:len(w)-2]
	}
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	if len(w) > 4 && strings.HasSuffix(w, "ed") {
		return w[:len(w)-2]
	}
	if len(w) > 4 && strings.HasSuffix(w, "ly") {
		return w[:len(w)-2]
	}
	return w
}

// BuildIndex constructs a BM25 index over chunks.
func BuildIndex(chunks []papers.Chunk) *Index {
	ix := &Index{Chunks: chunks, DF: map[string]float64{}, N: float64(len(chunks))}
	for _, c := range chunks {
		tf := map[string]float64{}
		for _, t := range Tokenize(c.Text) {
			tf[t]++
		}
		ix.TF = append(ix.TF, tf)
		ix.DocLen = append(ix.DocLen, float64(len(Tokenize(c.Text))))
		for t := range tf {
			ix.DF[t]++
		}
	}
	if ix.N > 0 {
		var total float64
		for _, d := range ix.DocLen {
			total += d
		}
		ix.AvgDL = total / ix.N
	}
	return ix
}

const k1 = 1.2
const b = 0.75

// Search returns the top-k chunks by BM25 score.
func (ix *Index) Search(query string, k int) []Scored {
	qTerms := Tokenize(query)
	if len(qTerms) == 0 || ix.N == 0 {
		return nil
	}
	scored := make([]Scored, 0, len(ix.Chunks))
	for i, c := range ix.Chunks {
		score := 0.0
		dl := ix.DocLen[i]
		tfMap := ix.TF[i]
		for _, qt := range qTerms {
			tf := tfMap[qt]
			if tf == 0 {
				continue
			}
			df := ix.DF[qt]
			idf := math.Log(1 + (ix.N-df+0.5)/(df+0.5))
			denom := tf + k1*(1-b+b*dl/ix.AvgDL)
			score += idf * (tf * (k1 + 1)) / denom
		}
		if score > 0 {
			scored = append(scored, Scored{Chunk: c, Score: score})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	return scored
}

// Rerank reorders candidates with a deterministic rescoring that rewards exact
// phrase matches, query-term coverage, table/prose plausibility and a paper
// hint. It is cheap (no model call) and is the setting tested in Table 3.
//
// paperHint, when not "any"/"", adds a bonus to chunks from that paper.
func RerankWithHint(query, paperHint string, cands []Scored) []Scored {
	qTerms := Tokenize(query)
	qSet := map[string]bool{}
	for _, t := range qTerms {
		qSet[t] = true
	}
	qPhrase := strings.Join(qTerms, " ")
	out := make([]Scored, len(cands))
	for i, c := range cands {
		norm := " " + strings.Join(Tokenize(c.Chunk.Text), " ") + " "
		bonus := 0.0
		if qPhrase != "" && strings.Contains(norm, " "+qPhrase+" ") {
			bonus += 6.0
		}
		coverage := 0.0
		for t := range qSet {
			if strings.Contains(norm, " "+t+" ") {
				coverage += 1.0
			}
		}
		if len(qSet) > 0 {
			bonus += 4.0 * coverage / float64(len(qSet))
		}
		// Title/section match bonus.
		titleTerms := Tokenize(c.Chunk.Title)
		for _, tt := range titleTerms {
			if qSet[tt] {
				bonus += 1.5
			}
		}
		// Paper hint from the planner: a strong but not absolute signal.
		if paperHint != "" && paperHint != "any" && c.Chunk.PaperID == paperHint {
			bonus += 3.0
		}
		// Penalize very short chunks (often headers/fragments).
		if c.Chunk.Words < 40 {
			bonus -= 2.0
		}
		// Prefer prose over table dumps. Tables in these PDFs come through as
		// long runs of bare numbers with almost no sentence punctuation, which
		// is exactly how a naive retriever ends up citing a table cell with no
		// explanatory text. Reward sentence-like text, penalize number density.
		bonus += proseScore(c.Chunk.Text)
		out[i] = Scored{Chunk: c.Chunk, Score: c.Score + bonus}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// Rerank is RerankWithHint with no paper hint.
func Rerank(query string, cands []Scored) []Scored {
	return RerankWithHint(query, "", cands)
}

// proseScore rewards sentence-like, explanatory text and penalizes dense runs
// of numbers (table dumps), in the range roughly [-3, +3].
func proseScore(text string) float64 {
	ws := strings.Fields(text)
	if len(ws) == 0 {
		return 0
	}
	numeric, alpha := 0, 0
	for _, w := range ws {
		core := strings.Trim(w, ".,;:()[]{}<>/|%$")
		if core == "" {
			continue
		}
		if isNumeric(core) {
			numeric++
			continue
		}
		if len(core) >= 2 && isLetter(core[0]) {
			alpha++
		}
	}
	frac := float64(numeric) / float64(len(ws))
	// Sentence punctuation density is another prose signal.
	sentences := strings.Count(text, ". ") + strings.Count(text, "; ")
	score := 0.0
	if frac < 0.05 {
		score += 1.5
	} else if frac < 0.12 {
		score += 0.0
	} else {
		score -= 3.0 * frac
	}
	if alpha > 60 {
		score += 1.0
	}
	if sentences >= 3 {
		score += 0.8
	}
	return score
}

func isNumeric(s string) bool {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.' || r == ',' || r == '-' || r == '+' || r == '×' || r == '−':
		default:
			return false
		}
	}
	return digits > 0
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
