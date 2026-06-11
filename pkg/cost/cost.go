// Package cost folds llm.usage events into a spend report — the tracking
// half of doc 22's "track and optimise agent costs". The llm node emits one
// terminal llm.usage per completion (tokens incl. cache, model binding, stop
// reason); this package prices those tokens against a per-model table and
// aggregates per request, per model, and in total.
//
// The price table is data, not code: defaults ship for the models the
// bootstrap uses, and the CLI accepts a JSON override file because prices
// drift and Model Garden models come and go. An unpriced model still gets
// token accounting — it is flagged, never silently costed at zero.
package cost

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
)

// Price is USD per million tokens, split the way providers bill: input,
// output, cache read, cache write.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// DefaultTable maps model-binding prefixes to prices. Longest-prefix match,
// so one row covers every snapshot of a model family. Anthropic rows follow
// the published per-MTok prices (cache read = 0.1x input, cache write =
// 1.25x input); Gemini / DeepSeek rows are Vertex list-price approximations —
// override via a JSON table when precision matters.
var DefaultTable = map[string]Price{
	"vertex:anthropic/claude-opus":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"vertex:anthropic/claude-sonnet": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"vertex:anthropic/claude-haiku":  {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	"vertex:gemini-2.5-pro":          {Input: 1.25, Output: 10, CacheRead: 0.31, CacheWrite: 1.625},
	"vertex:gemini-2.5-flash":        {Input: 0.30, Output: 2.50, CacheRead: 0.075, CacheWrite: 0.375},
	"vertex:gemini-3.5-flash":        {Input: 0.30, Output: 2.50, CacheRead: 0.075, CacheWrite: 0.375},
	"vertex:deepseek/deepseek-r1":    {Input: 1.35, Output: 5.40},
}

// Lookup resolves a model binding against the table by longest prefix.
func Lookup(model string, table map[string]Price) (Price, bool) {
	bestLen := -1
	var best Price
	for prefix, p := range table {
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			bestLen = len(prefix)
			best = p
		}
	}
	return best, bestLen >= 0
}

// Usage is one llm.usage event, decoded.
type Usage struct {
	RequestID           string `json:"-"`
	Model               string `json:"model"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
}

// USD prices one usage record against the table. ok is false when the model
// has no row — tokens still count, dollars don't.
func (u Usage) USD(table map[string]Price) (float64, bool) {
	p, ok := Lookup(u.Model, table)
	if !ok {
		return 0, false
	}
	const m = 1e6
	return float64(u.InputTokens)/m*p.Input +
		float64(u.OutputTokens)/m*p.Output +
		float64(u.CacheReadTokens)/m*p.CacheRead +
		float64(u.CacheCreationTokens)/m*p.CacheWrite, true
}

// Line is one aggregated row of the report.
type Line struct {
	Calls               int     `json:"calls"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	USD                 float64 `json:"usd"`
	// Unpriced counts calls whose model had no price row; their tokens are
	// in the line but their dollars are not.
	Unpriced int `json:"unpriced,omitempty"`
}

func (l *Line) add(u Usage, table map[string]Price) {
	l.Calls++
	l.InputTokens += u.InputTokens
	l.OutputTokens += u.OutputTokens
	l.CacheReadTokens += u.CacheReadTokens
	l.CacheCreationTokens += u.CacheCreationTokens
	if usd, ok := u.USD(table); ok {
		l.USD += usd
	} else {
		l.Unpriced++
	}
}

// Report is the full aggregation.
type Report struct {
	Total     Line            `json:"total"`
	ByModel   map[string]Line `json:"by_model"`
	ByRequest map[string]Line `json:"by_request"`
}

// Aggregate folds usage records into a report.
func Aggregate(usages []Usage, table map[string]Price) Report {
	r := Report{ByModel: map[string]Line{}, ByRequest: map[string]Line{}}
	for _, u := range usages {
		r.Total.add(u, table)
		lm := r.ByModel[u.Model]
		lm.add(u, table)
		r.ByModel[u.Model] = lm
		lr := r.ByRequest[u.RequestID]
		lr.add(u, table)
		r.ByRequest[u.RequestID] = lr
	}
	return r
}

// jsonlLine is the common shape of both supported log formats: `--trace`
// output (full event.Event) and the audit handler's sink lines — both carry
// type, request_id and a payload object.
type jsonlLine struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Payload   json.RawMessage `json:"payload"`
}

// ReadUsageJSONL scans a JSONL stream and returns the decoded llm.usage
// events, skipping everything else (including unparseable lines — a usage
// fold should not die on an interleaved log line).
func ReadUsageJSONL(r io.Reader) ([]Usage, error) {
	var out []Usage
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var l jsonlLine
		if err := json.Unmarshal(line, &l); err != nil || l.Type != "llm.usage" {
			continue
		}
		var u Usage
		if err := json.Unmarshal(l.Payload, &u); err != nil {
			continue
		}
		u.RequestID = l.RequestID
		out = append(out, u)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cost: read jsonl: %w", err)
	}
	return out, nil
}

// LoadTable reads a JSON price-table override ({"prefix": {input, output,
// cache_read, cache_write}, ...}) and merges it over the defaults — an
// override row replaces the default row for the same prefix.
func LoadTable(r io.Reader) (map[string]Price, error) {
	var override map[string]Price
	if err := json.NewDecoder(r).Decode(&override); err != nil {
		return nil, fmt.Errorf("cost: parse price table: %w", err)
	}
	table := make(map[string]Price, len(DefaultTable)+len(override))
	maps.Copy(table, DefaultTable)
	maps.Copy(table, override)
	return table, nil
}

// Render writes the report as an aligned text table: totals, per-model
// breakdown, and (optionally) per-request breakdown.
func Render(w io.Writer, r Report, byRequest bool) {
	writeLine := func(label string, l Line) {
		fmt.Fprintf(w, "%-44s %6d %12d %12d %12d %12d %10.4f", label, l.Calls,
			l.InputTokens, l.OutputTokens, l.CacheReadTokens, l.CacheCreationTokens, l.USD)
		if l.Unpriced > 0 {
			fmt.Fprintf(w, "  (%d unpriced)", l.Unpriced)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%-44s %6s %12s %12s %12s %12s %10s\n", "", "calls", "in", "out", "cache_rd", "cache_wr", "usd")
	for _, model := range sortedKeys(r.ByModel) {
		writeLine(model, r.ByModel[model])
	}
	if byRequest {
		fmt.Fprintln(w)
		for _, req := range sortedKeys(r.ByRequest) {
			writeLine(req, r.ByRequest[req])
		}
	}
	fmt.Fprintln(w)
	writeLine("TOTAL", r.Total)
}

func sortedKeys(m map[string]Line) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
