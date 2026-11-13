// Package eval grades pipeline answers against my hand-written ground truth
// and provides shared result-row plumbing for results.csv.
package eval

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Question is one graded question.
type Question struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Expect string `json:"expect"` // correct, refused, adversarial
	Paper  string `json:"paper"`
	Why    string `json:"why"`
	// Gold is the statement in the paper that answers the question, as a list
	// of regular expressions that must ALL appear in the retrieved context.
	// It measures the retrieval step alone (no model call), so quality can be
	// compared across settings without spending the daily token budget.
	Gold []string `json:"gold,omitempty"`
}

// QuestionSet is the JSON shape of data/questions.json.
type QuestionSet struct {
	Note      string     `json:"note"`
	Questions []Question `json:"questions"`
}

// Row is one row of results.csv.
type Row struct {
	RequestID        string
	Scenario         string
	Source           string // real | stand-in
	StartTime        string
	FirstTokenMs     float64
	TotalMs          float64
	Status           string // ok, rate_limited, busy, timeout, error
	Retries          int
	TokensIn         int
	TokensOut        int
	RetrievalSetting string
	AnswerGrade      string // correct, wrong, refused_correctly, refused_wrongly
}

// Columns is the required header order from the brief.
var Columns = []string{
	"request_id", "scenario", "source", "start_time", "first_token_ms",
	"total_ms", "status", "retries", "tokens_in", "tokens_out",
	"retrieval_setting", "answer_grade",
}

// ReadCSV reads existing rows so a load run can append to a shared file.
func ReadCSV(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var rows []Row
	for i, rec := range recs {
		if i == 0 || len(rec) < len(Columns) {
			continue // header or malformed
		}
		rows = append(rows, Row{
			RequestID: rec[0], Scenario: rec[1], Source: rec[2], StartTime: rec[3],
			FirstTokenMs: atof(rec[4]), TotalMs: atof(rec[5]), Status: rec[6],
			Retries: atoi(rec[7]), TokensIn: atoi(rec[8]), TokensOut: atoi(rec[9]),
			RetrievalSetting: rec[10], AnswerGrade: rec[11],
		})
	}
	return rows, nil
}

// WriteCSV writes rows to path with the required columns.
func WriteCSV(path string, rows []Row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(Columns); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{
			r.RequestID, r.Scenario, r.Source, r.StartTime,
			fmt.Sprintf("%.1f", r.FirstTokenMs),
			fmt.Sprintf("%.1f", r.TotalMs),
			r.Status,
			strconv.Itoa(r.Retries),
			strconv.Itoa(r.TokensIn),
			strconv.Itoa(r.TokensOut),
			r.RetrievalSetting,
			r.AnswerGrade,
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// Now returns an ISO-8601 timestamp for start_time.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// NormalizeStatus maps a provider status to the allowed set.
func NormalizeStatus(s string) string {
	switch s {
	case "ok", "rate_limited", "busy", "timeout", "error":
		return s
	}
	return "error"
}

// Grade decides a grade from the expected class and the answer.
//
// The four allowed grades are coarse on purpose: this grader cannot verify a
// fact, only whether the assistant answered with a citation or refused. The
// *fact* correctness of each answer was checked by me by hand (see
// results/quality_manual.md); this function records the automatic part:
//
//   - expect "refused": the papers do not answer it, so a refusal is correct
//     and any answer is wrong (hallucination).
//   - expect "correct"/"adversarial": a refusal is refused_wrongly; an answer
//     with at least one citation is "correct" here and was hand-verified;
//     an answer with no citation is wrong.
//
// It deliberately does not fail an answer for citing a different but valid
// page of the same paper; page precision is reported separately in Table 3.
func Grade(expect string, refused bool, hasCitation bool, textContainsNotInLib bool) string {
	if expect == "refused" {
		if refused || textContainsNotInLib {
			return "refused_correctly"
		}
		return "wrong"
	}
	if refused || textContainsNotInLib {
		return "refused_wrongly"
	}
	if !hasCitation {
		return "wrong"
	}
	return "correct"
}

// GradeWithFact lets the manual fact verdict override the automatic grade, so
// that "correct fact, imperfect page" and "wrong fact" are scored honestly.
func GradeWithFact(expect string, refused bool, hasCitation bool, textContainsNotInLib bool, factCorrect *bool) string {
	if expect == "refused" {
		return Grade(expect, refused, hasCitation, textContainsNotInLib)
	}
	if refused || textContainsNotInLib {
		return "refused_wrongly"
	}
	if !hasCitation {
		return "wrong"
	}
	if factCorrect != nil && !*factCorrect {
		return "wrong"
	}
	return "correct"
}

// SettingLabel is a stable label used in results.csv and Table 3.
func SettingLabel(piece, k int, rerank bool) string {
	r := "no"
	if rerank {
		r = "yes"
	}
	return fmt.Sprintf("piece=%d,k=%d,rerank=%s", piece, k, r)
}

// Slug turns a setting into a filesystem-safe token.
func Slug(s string) string {
	s = strings.ReplaceAll(s, "piece=", "p")
	s = strings.ReplaceAll(s, ",k=", "_k")
	s = strings.ReplaceAll(s, ",rerank=", "_r")
	return s
}
