package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"

	"relay/internal/retrieval"
)

// searchArgs is the argument object of the one tool the pipeline executes.
// The same shape is advertised in searchTool's JSON schema, so an external
// caller and the planner cannot drift apart.
type searchArgs struct {
	Query string `json:"query"`
	Paper string `json:"paper,omitempty"`
}

// invokeTool is the single dispatch point for tool calls. The planner chooses
// the arguments; the schema says which fields exist; this function validates
// them and runs the retrieval. Keeping the call here (rather than inlining
// p.retrieve in Run) makes the tool boundary explicit in the code and gives a
// model-emitted call the same path as the pipeline's own.
func (p *Pipeline) invokeTool(name string, raw json.RawMessage) ([]retrieval.Scored, float64, error) {
	if name != searchTool.Name {
		return nil, 0, fmt.Errorf("unknown tool %q", name)
	}
	var a searchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, 0, fmt.Errorf("%s: bad arguments: %w", name, err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, 0, fmt.Errorf("%s: empty query", name)
	}
	hits, coverage := p.retrieve(a.Query, a.Paper)
	return hits, coverage, nil
}
