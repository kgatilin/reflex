package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidate_HealthyTopologyIsConnected(t *testing.T) {
	rep, err := Validate(exampleTopology()...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Connected {
		t.Fatalf("expected Connected==true; got gaps:\n"+
			"  dead-ends: %v\n  unreachable: %v\n  fragments: %v\n  unbounded cycles: %v",
			rep.DeadEnds, rep.UnreachableNodes, rep.Fragments, rep.UnboundedCycles)
	}
}

func TestValidate_RemovingNotifyMakesTaskAnsweredADeadEnd(t *testing.T) {
	// Mutation: drop the notify sink. task.answered and task.needs_clarification
	// lose their only consumer and become dead-ends; the report flags them with
	// a bridge suggestion.
	var mutated []Decl
	for _, d := range exampleTopology() {
		if n, ok := d.(Node); ok && n.Name == "notify" {
			continue
		}
		mutated = append(mutated, d)
	}

	rep, err := Validate(mutated...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Connected {
		t.Fatal("expected Connected==false after removing notify")
	}
	if !contains(rep.DeadEnds, "task.answered") {
		t.Fatalf("expected task.answered in dead-ends; got %v", rep.DeadEnds)
	}
	if !contains(rep.DeadEnds, "task.needs_clarification") {
		t.Fatalf("expected task.needs_clarification in dead-ends; got %v", rep.DeadEnds)
	}
	if !anyContains(rep.Suggestions, "task.answered") ||
		!anyContains(rep.Suggestions, "add an llm node") {
		t.Fatalf("expected an llm-bridge suggestion for task.answered; got %v", rep.Suggestions)
	}
}

func TestValidate_LLMOnlyCycleIsUnbounded(t *testing.T) {
	// An ingress root reaches a two-node llm↔llm loop with no deterministic
	// guard: the SCC {a,b} is a real cycle with nothing to force an exit, so it
	// must be reported as unbounded (doc 26 §3a / doc 27 §5).
	decls := []Decl{
		Node{
			Name:  "in",
			Kind:  KindDeterministic,
			On:    []string{"app.ingress.*"},
			In:    "global",
			Emits: []string{"kind.a"},
		},
		Node{
			Name:  "a",
			Kind:  KindLLM,
			On:    []string{"kind.a", "kind.b"},
			In:    "request",
			Emits: []string{"kind.b"},
		},
		Node{
			Name:  "b",
			Kind:  KindLLM,
			On:    []string{"kind.b"},
			In:    "request",
			Emits: []string{"kind.a"},
		},
	}

	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(rep.UnboundedCycles) == 0 {
		t.Fatal("expected an unbounded cycle for the llm↔llm loop")
	}
	found := false
	for _, scc := range rep.UnboundedCycles {
		if contains(scc, "a") && contains(scc, "b") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected SCC {a,b} reported unbounded; got %v", rep.UnboundedCycles)
	}
	if rep.Connected {
		t.Fatal("expected Connected==false with an unbounded cycle")
	}
}

func TestValidate_DeterministicGuardBoundsTheCycle(t *testing.T) {
	// Same loop, but add a deterministic guard inside the SCC: now it is bounded
	// and not reported.
	decls := []Decl{
		Node{Name: "in", Kind: KindDeterministic, On: []string{"app.ingress.*"}, In: "global", Emits: []string{"kind.a"}},
		Node{Name: "a", Kind: KindLLM, On: []string{"kind.a", "kind.b"}, In: "request", Emits: []string{"kind.b"}},
		Node{Name: "b", Kind: KindLLM, On: []string{"kind.b"}, In: "request", Emits: []string{"kind.a", "kind.done"}},
		// guard sits in the SCC: consumes kind.b, re-emits kind.a (the loop
		// edge), so it is mutually reachable with a and b.
		Node{Name: "guard", Kind: KindDeterministic, On: []string{"kind.b"}, In: "request", Emits: []string{"kind.a"}},
		// done is consumed so it is not a dead-end.
		Node{Name: "sink", Kind: KindDeterministic, On: []string{"kind.done"}, In: "request"},
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(rep.UnboundedCycles) != 0 {
		t.Fatalf("expected no unbounded cycles with a deterministic guard; got %v", rep.UnboundedCycles)
	}
}

func TestApply_RoutesThroughValidate(t *testing.T) {
	e := New()
	if err := e.Apply(context.Background(), exampleTopology()...); err != nil {
		t.Fatalf("Apply healthy topology: %v", err)
	}

	// A topology with a dead-end fails Apply with a ValidationError carrying the
	// Report.
	bad := []Decl{
		Node{Name: "in", Kind: KindDeterministic, On: []string{"app.ingress.*"}, In: "global", Emits: []string{"orphan.kind"}},
	}
	err := e.Apply(context.Background(), bad...)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError; got %T: %v", err, err)
	}
	if !contains(ve.Report.DeadEnds, "orphan.kind") {
		t.Fatalf("expected orphan.kind dead-end in report; got %v", ve.Report.DeadEnds)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func anyContains(ss []string, sub string) bool {
	for _, x := range ss {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}
