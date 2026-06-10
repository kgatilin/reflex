package graph

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// triageYAML mirrors examples/triage.yaml (minus the unhandled watcher's
// noop subscription which doesn't contribute edges).
const triageYAML = `
handlers:
  - name: parse
    type: parse_target
    on: RequestReceived
  - name: fetch_comments
    type: gh_query
    on: TargetParsed
    config: { path: comments }
  - name: fetch_timeline
    type: gh_query
    on: TargetParsed
    config: { path: timeline }
  - name: classify
    type: triage_rules
    on: GhQueryResult
  - name: announce
    type: printer
    on: TriageDecided
  - name: finalize
    type: terminator
    on: TriageDecided
  - name: watcher
    type: unhandled_watcher
    on: __noop__
`

func TestBuildTriageGraphHasExpectedEdges(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(triageYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(g.Nodes) != 7 {
		t.Fatalf("nodes = %d, want 7", len(g.Nodes))
	}
	// Expected edges (non-exhaustive — assert the key ones):
	//   parse → fetch_comments via TargetParsed
	//   parse → fetch_timeline via TargetParsed
	//   fetch_comments → classify via GhQueryResult
	//   fetch_timeline → classify via GhQueryResult
	//   classify → announce via TriageDecided
	//   classify → finalize via TriageDecided
	want := []struct{ from, to, ev string }{
		{"parse", "fetch_comments", "TargetParsed"},
		{"parse", "fetch_timeline", "TargetParsed"},
		{"fetch_comments", "classify", "GhQueryResult"},
		{"fetch_timeline", "classify", "GhQueryResult"},
		{"classify", "announce", "TriageDecided"},
		{"classify", "finalize", "TriageDecided"},
	}
	for _, w := range want {
		if !hasEdge(g.Edges, w.from, w.to, w.ev) {
			t.Errorf("missing edge %s -> %s [%s]", w.from, w.to, w.ev)
		}
	}
	if len(g.Cycles) != 0 {
		t.Fatalf("triage has cycles: %v", g.Cycles)
	}
}

func TestBuildDescribeOutputMentionsAllHandlers(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(triageYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var buf bytes.Buffer
	if err := g.Describe(&buf); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, name := range []string{"parse", "fetch_comments", "fetch_timeline", "classify", "announce", "finalize", "watcher"} {
		if !strings.Contains(buf.String(), name) {
			t.Errorf("describe output missing %q\n%s", name, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "7 handlers") {
		t.Errorf("describe output missing handler count\n%s", buf.String())
	}
}

func TestBuildEchoEmitTypeFromConfig(t *testing.T) {
	// echo's Emits depend on config.emit — verify the SpecResolver path.
	yamlSrc := `
handlers:
  - name: bounce
    type: echo
    on: RequestReceived
    config: { emit: AssistantMessageProposed }
  - name: out
    type: printer
    on: AssistantMessageProposed
  - name: term
    type: terminator
    on: AssistantMessageProposed
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !hasEdge(g.Edges, "bounce", "out", "AssistantMessageProposed") {
		t.Fatalf("echo→printer edge missing; edges: %+v", g.Edges)
	}
}

func hasEdge(edges []HandlerEdge, from, to, ev string) bool {
	for _, e := range edges {
		if e.From == from && e.To == to && e.EventType == ev {
			return true
		}
	}
	return false
}
