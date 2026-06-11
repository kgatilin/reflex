package cost

import (
	"math"
	"strings"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLookupLongestPrefix(t *testing.T) {
	table := map[string]Price{
		"vertex:anthropic/claude":      {Input: 1},
		"vertex:anthropic/claude-opus": {Input: 5},
	}
	p, ok := Lookup("vertex:anthropic/claude-opus-4-8", table)
	if !ok || p.Input != 5 {
		t.Errorf("longest prefix not preferred: %+v ok=%v", p, ok)
	}
	p, ok = Lookup("vertex:anthropic/claude-sonnet-4-6", table)
	if !ok || p.Input != 1 {
		t.Errorf("fallback prefix: %+v ok=%v", p, ok)
	}
	if _, ok := Lookup("vertex:llama/llama-4", table); ok {
		t.Error("unknown model must not match")
	}
}

func TestUSD(t *testing.T) {
	u := Usage{
		Model:               "vertex:anthropic/claude-opus-4-8",
		InputTokens:         1_000_000,
		OutputTokens:        100_000,
		CacheReadTokens:     2_000_000,
		CacheCreationTokens: 400_000,
	}
	usd, ok := u.USD(DefaultTable)
	if !ok {
		t.Fatal("opus must be priced by the default table")
	}
	// 1M*5 + 0.1M*25 + 2M*0.5 + 0.4M*6.25 = 5 + 2.5 + 1 + 2.5 = 11
	if !almostEqual(usd, 11.0) {
		t.Errorf("usd = %v, want 11.0", usd)
	}

	if _, ok := (Usage{Model: "vertex:mystery/model-x"}).USD(DefaultTable); ok {
		t.Error("unknown model must report unpriced")
	}
}

func TestAggregate(t *testing.T) {
	usages := []Usage{
		{RequestID: "r1", Model: "vertex:anthropic/claude-opus-4-8", InputTokens: 100, OutputTokens: 10},
		{RequestID: "r1", Model: "vertex:anthropic/claude-opus-4-8", InputTokens: 200, OutputTokens: 20},
		{RequestID: "r2", Model: "vertex:mystery/model-x", InputTokens: 50},
	}
	r := Aggregate(usages, DefaultTable)
	if r.Total.Calls != 3 || r.Total.InputTokens != 350 || r.Total.OutputTokens != 30 {
		t.Errorf("total = %+v", r.Total)
	}
	if r.Total.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", r.Total.Unpriced)
	}
	if got := r.ByRequest["r1"]; got.Calls != 2 || got.InputTokens != 300 {
		t.Errorf("r1 = %+v", got)
	}
	if got := r.ByModel["vertex:mystery/model-x"]; got.Calls != 1 || got.Unpriced != 1 {
		t.Errorf("mystery = %+v", got)
	}
}

func TestReadUsageJSONL(t *testing.T) {
	// A mix of trace-style event lines, audit-style lines, noise, and junk.
	log := strings.Join([]string{
		`{"id":"e1","type":"RequestReceived","request_id":"r1","payload":{"payload":"fix it"}}`,
		`{"id":"e2","type":"llm.usage","request_id":"r1","payload":{"model":"vertex:anthropic/claude-opus-4-8","input_tokens":120,"output_tokens":30,"cache_read_tokens":40,"cache_creation_tokens":5,"stop_reason":"tool_use"}}`,
		`not json at all`,
		`{"type":"llm.usage","request_id":"r2","ts":"2026-06-12T10:00:00Z","payload":{"model":"vertex:gemini-2.5-flash","input_tokens":10,"output_tokens":2}}`,
		`{"type":"tool.fs.read.result","request_id":"r1","payload":{"path":"a.go"}}`,
	}, "\n")
	usages, err := ReadUsageJSONL(strings.NewReader(log))
	if err != nil {
		t.Fatalf("ReadUsageJSONL: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("usages = %+v", usages)
	}
	if usages[0].RequestID != "r1" || usages[0].InputTokens != 120 || usages[0].CacheReadTokens != 40 {
		t.Errorf("first = %+v", usages[0])
	}
	if usages[1].Model != "vertex:gemini-2.5-flash" {
		t.Errorf("second = %+v", usages[1])
	}
}

func TestLoadTableMergesOverDefaults(t *testing.T) {
	override := `{"vertex:anthropic/claude-opus": {"input": 7, "output": 30},
	              "vertex:llama/llama-4": {"input": 0.2, "output": 0.6}}`
	table, err := LoadTable(strings.NewReader(override))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if p, _ := Lookup("vertex:anthropic/claude-opus-4-8", table); p.Input != 7 {
		t.Errorf("override not applied: %+v", p)
	}
	if p, ok := Lookup("vertex:anthropic/claude-haiku-4-5", table); !ok || p.Input != 1 {
		t.Errorf("default row lost: %+v ok=%v", p, ok)
	}
	if _, ok := Lookup("vertex:llama/llama-4-x", table); !ok {
		t.Error("new row missing")
	}
}

func TestRender(t *testing.T) {
	r := Aggregate([]Usage{
		{RequestID: "r1", Model: "vertex:anthropic/claude-opus-4-8", InputTokens: 1_000_000, OutputTokens: 100_000},
	}, DefaultTable)
	var b strings.Builder
	Render(&b, r, true)
	out := b.String()
	for _, want := range []string{"vertex:anthropic/claude-opus-4-8", "TOTAL", "r1", "7.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}
