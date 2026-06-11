package graph

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// fanGraphYAML is a synthetic fan-out/fan-in DAG built from the surviving
// echo/printer/terminator handlers: a source step parses, two parallel
// fetch steps fan out, a classify step fans them back in, and two sink
// steps (announce + finalize) close the branch. It exercises the graph
// builder's edge resolution, fan-out/fan-in, and cycle check.
const fanGraphYAML = `
handlers:
  - name: parse
    type: echo
    on: RequestReceived
    config: { emit: StepParsed }
  - name: fetch_a
    type: echo
    on: StepParsed
    config: { emit: StepFetched }
  - name: fetch_b
    type: echo
    on: StepParsed
    config: { emit: StepFetched }
  - name: classify
    type: echo
    on: StepFetched
    config: { emit: StepDecided }
  - name: announce
    type: printer
    on: StepDecided
  - name: finalize
    type: terminator
    on: StepDecided
  - name: watcher
    type: unhandled_watcher
    on: __noop__
`

func TestBuildFanGraphHasExpectedEdges(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(fanGraphYAML), reg.Types())
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
	//   parse → fetch_a via StepParsed
	//   parse → fetch_b via StepParsed
	//   fetch_a → classify via StepFetched
	//   fetch_b → classify via StepFetched
	//   classify → announce via StepDecided
	//   classify → finalize via StepDecided
	want := []struct{ from, to, ev string }{
		{"parse", "fetch_a", "StepParsed"},
		{"parse", "fetch_b", "StepParsed"},
		{"fetch_a", "classify", "StepFetched"},
		{"fetch_b", "classify", "StepFetched"},
		{"classify", "announce", "StepDecided"},
		{"classify", "finalize", "StepDecided"},
	}
	for _, w := range want {
		if !hasEdge(g.Edges, w.from, w.to, w.ev) {
			t.Errorf("missing edge %s -> %s [%s]", w.from, w.to, w.ev)
		}
	}
	if len(g.Cycles) != 0 {
		t.Fatalf("fan graph has cycles: %v", g.Cycles)
	}
}

func TestBuildDescribeOutputMentionsAllHandlers(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(fanGraphYAML), reg.Types())
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
	for _, name := range []string{"parse", "fetch_a", "fetch_b", "classify", "announce", "finalize", "watcher"} {
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
