package graph

import (
	"errors"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

func TestAcyclicConfigDetectsZeroCycles(t *testing.T) {
	yamlSrc := `
handlers:
  - name: a
    type: echo
    on: RequestReceived
    config: { emit: TargetParsed }
  - name: b
    type: terminator
    on: TargetParsed
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(g.Cycles) != 0 {
		t.Fatalf("expected zero cycles, got %v", g.Cycles)
	}
}

func TestUncappedCycleRejected(t *testing.T) {
	yamlSrc := `
handlers:
  - name: ping
    type: echo
    on: PongEvent
    config: { emit: PingEvent }
  - name: pong
    type: echo
    on: PingEvent
    config: { emit: PongEvent }
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(cfg, reg)
	if err == nil {
		t.Fatal("expected uncapped cycle to be rejected")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *CycleError; err = %v", err, err)
	}
	if len(ce.Cycles) == 0 {
		t.Fatalf("CycleError has no cycles")
	}
	if !strings.Contains(err.Error(), "ping") || !strings.Contains(err.Error(), "pong") {
		t.Fatalf("cycle error doesn't mention both nodes: %v", err)
	}
}

func TestCappedCycleAccepted(t *testing.T) {
	yamlSrc := `
handlers:
  - name: ping
    type: echo
    on: PongEvent
    config: { emit: PingEvent }
    loop:
      max_iterations: 3
  - name: pong
    type: echo
    on: PingEvent
    config: { emit: PongEvent }
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(g.Cycles) != 1 {
		t.Fatalf("cycles = %v, want 1", g.Cycles)
	}
	caps := g.Caps()
	if caps["ping"] != 3 {
		t.Fatalf("caps[ping] = %d, want 3", caps["ping"])
	}
}

func TestThreeNodeCycleDetected(t *testing.T) {
	yamlSrc := `
handlers:
  - name: a
    type: echo
    on: CEvent
    config: { emit: AEvent }
  - name: b
    type: echo
    on: AEvent
    config: { emit: BEvent }
  - name: c
    type: echo
    on: BEvent
    config: { emit: CEvent }
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(cfg, reg)
	if err == nil {
		t.Fatal("expected uncapped 3-node cycle to be rejected")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want CycleError, got %T", err)
	}
	if len(ce.Cycles) != 1 {
		t.Fatalf("cycles = %v, want 1", ce.Cycles)
	}
	cyc := ce.Cycles[0]
	if len(cyc) != 3 {
		t.Fatalf("cycle len = %d, want 3", len(cyc))
	}
}

func TestNestedCyclesHandled(t *testing.T) {
	// Two disjoint SCCs in the same config; one capped, one not.
	yamlSrc := `
handlers:
  - name: a1
    type: echo
    on: A2Event
    config: { emit: A1Event }
    loop:
      max_iterations: 2
  - name: a2
    type: echo
    on: A1Event
    config: { emit: A2Event }
  - name: b1
    type: echo
    on: B2Event
    config: { emit: B1Event }
  - name: b2
    type: echo
    on: B1Event
    config: { emit: B2Event }
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(cfg, reg)
	if err == nil {
		t.Fatal("expected uncapped B-cycle to be rejected even though A is capped")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want CycleError, got %T", err)
	}
	// Only the uncapped (b1, b2) cycle should be in the error.
	if len(ce.Cycles) != 1 {
		t.Fatalf("uncapped cycles = %d, want 1; got %v", len(ce.Cycles), ce.Cycles)
	}
	if !strings.Contains(ce.Cycles[0][0], "b") {
		t.Fatalf("expected b-cycle in error, got %v", ce.Cycles)
	}
}

func TestTerminalEmissionDoesNotCloseCycle(t *testing.T) {
	// terminator emits Terminal RequestHandled — even though the spec lists
	// it, the static graph should not treat that edge as cycle-forming.
	yamlSrc := `
handlers:
  - name: closer
    type: terminator
    on: RequestHandled
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlSrc), reg.Types())
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v; cycles = %v", err, g.Cycles)
	}
	if len(g.Cycles) != 0 {
		t.Fatalf("terminal self-loop was treated as cycle: %v", g.Cycles)
	}
}
