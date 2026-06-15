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
		if n, ok := d.(Subscriber); ok && n.Name == "notify" {
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

func TestValidate_UnbudgetedCycleIsUnbounded(t *testing.T) {
	// An external root reaches a two-node loop whose nodes sit in a scope with no
	// declared budget (here "request", which no Scope budgets): the SCC {a,b} is
	// a real cycle with no scope budget to force an exit, so it must be reported
	// as unbounded (doc 24 §5 "loops are budgets"). Node determinism is
	// irrelevant — termination is a scope property, not a node property.
	decls := []Decl{
		Subscriber{
			Name:  "in",
			On:    []string{"cli.task"},
			In:    "global",
			Emits: []string{"kind.a"},
		},
		Subscriber{
			Name:  "a",
			On:    []string{"kind.a", "kind.b"},
			In:    "request",
			Emits: []string{"kind.b"},
		},
		Subscriber{
			Name:  "b",
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
		t.Fatal("expected an unbounded cycle for the unbudgeted loop")
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

func TestValidate_BudgetedScopeBoundsTheCycle(t *testing.T) {
	// Same loop, but declare a budgeted scope "loop" covering the cycle nodes:
	// the scope budget bounds a kind's count within the cone, so the SCC is
	// bounded and not reported (doc 24 §5 "loops are budgets").
	decls := []Decl{
		Scope{Name: "loop", Root: "kind.a", Budget: map[string]int{"kind.a": 8}},
		Subscriber{Name: "in", On: []string{"cli.task"}, In: "global", Emits: []string{"kind.a"}},
		Subscriber{Name: "a", On: []string{"kind.a", "kind.b"}, In: "loop", Emits: []string{"kind.b"}},
		Subscriber{Name: "b", On: []string{"kind.b"}, In: "loop", Emits: []string{"kind.a", "kind.done"}},
		// done is consumed so it is not a dead-end.
		Subscriber{Name: "sink", On: []string{"kind.done"}, In: "loop"},
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(rep.UnboundedCycles) != 0 {
		t.Fatalf("expected no unbounded cycles within a budgeted scope; got %v", rep.UnboundedCycles)
	}
}

func TestValidate_StalledClosureNeedsAConsumer(t *testing.T) {
	// A declared scope whose scope.X.closed has no consumer is a stalled
	// closure (doc 26 §3f / 27 §5): the cone can freeze in the void. The
	// resolver roots the scope; nothing reads scope.work.closed.
	decls := []Decl{
		Scope{Name: "work", Root: "request.received", Budget: map[string]int{"tool.x.call": 4}},
		Subscriber{Name: "resolver", On: []string{"cli.task"}, In: "global", Emits: []string{"request.received"}},
		Subscriber{Name: "doer", On: []string{"request.received"}, In: "work", Emits: []string{"tool.x.call"}},
		Subscriber{Name: "tool", On: []string{"tool.x.call"}, In: "work", Emits: []string{"tool.x.result"}},
		Subscriber{Name: "loop", On: []string{"tool.x.result"}, In: "work", Emits: []string{"tool.x.call"}},
		EventKind{Kind: "cli.task"}, EventKind{Kind: "request.received"},
		EventKind{Kind: "tool.x.call"}, EventKind{Kind: "tool.x.result"},
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !contains(rep.StalledClosures, "work") {
		t.Fatalf("expected \"work\" in stalled closures; got %v", rep.StalledClosures)
	}
	if rep.Connected {
		t.Fatal("expected Connected==false with a stalled closure")
	}
	if !anyContains(rep.Suggestions, "scope.work.closed") || !anyContains(rep.Suggestions, "bridge") {
		t.Fatalf("expected a bridge suggestion for scope.work.closed; got %v", rep.Suggestions)
	}

	// Add a consumer of scope.work.closed (a terminator): the gap closes.
	bridged := append(decls, Subscriber{Name: "terminator", On: []string{"scope.work.closed"}})
	rep2, err := Validate(bridged...)
	if err != nil {
		t.Fatalf("Validate (bridged): %v", err)
	}
	if len(rep2.StalledClosures) != 0 {
		t.Fatalf("expected no stalled closures once scope.work.closed has a consumer; got %v", rep2.StalledClosures)
	}
	if !rep2.Connected {
		t.Fatalf("expected Connected==true once bridged; gaps: dead=%v unreach=%v cycles=%v stalled=%v",
			rep2.DeadEnds, rep2.UnreachableNodes, rep2.UnboundedCycles, rep2.StalledClosures)
	}
}

func TestValidate_CoRootedScopesRejected(t *testing.T) {
	// Two scopes rooting on the same event (both Root "request.received") would
	// open two instances on one span — a degenerate co-rooting forbidden by the
	// model (doc 24 §5 / 26 §3d). The validator rejects it and suggests merging
	// into one scope with both budgets in its Budget map.
	decls := []Decl{
		Scope{Name: "request", Root: "request.received", Budget: map[string]int{"llm.call": 10}},
		Scope{Name: "cost", Root: "request.received", Budget: map[string]int{"tool.pay.call": 3}},
		Subscriber{Name: "resolver", On: []string{"cli.task"}, In: "global", Emits: []string{"request.received"}},
		Subscriber{Name: "work", On: []string{"request.received"}, In: "request", Emits: []string{"task.answered"}},
		Subscriber{Name: "notify", On: []string{"task.answered", "scope.request.closed", "scope.cost.closed"}, In: "global"},
		EventKind{Kind: "cli.task"}, EventKind{Kind: "request.received"}, EventKind{Kind: "task.answered"},
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	found := false
	for _, pair := range rep.CoRootedScopes {
		if contains(pair, "request") && contains(pair, "cost") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected {cost,request} reported as co-rooted; got %v", rep.CoRootedScopes)
	}
	if rep.Connected {
		t.Fatal("expected Connected==false with co-rooted scopes")
	}
	if !anyContains(rep.Suggestions, "root on the same event") || !anyContains(rep.Suggestions, "Budget map") {
		t.Fatalf("expected a merge suggestion for co-rooted scopes; got %v", rep.Suggestions)
	}

	// One scope carrying BOTH budgets is the sanctioned form — no co-rooting.
	merged := []Decl{
		Scope{Name: "request", Root: "request.received", Budget: map[string]int{"llm.call": 10, "tool.pay.call": 3}},
		Subscriber{Name: "resolver", On: []string{"cli.task"}, In: "global", Emits: []string{"request.received"}},
		Subscriber{Name: "work", On: []string{"request.received"}, In: "request", Emits: []string{"task.answered"}},
		Subscriber{Name: "notify", On: []string{"task.answered", "scope.request.closed"}, In: "global"},
		EventKind{Kind: "cli.task"}, EventKind{Kind: "request.received"}, EventKind{Kind: "task.answered"},
	}
	rep2, err := Validate(merged...)
	if err != nil {
		t.Fatalf("Validate (merged): %v", err)
	}
	if len(rep2.CoRootedScopes) != 0 {
		t.Fatalf("expected no co-rooting with one scope carrying both budgets; got %v", rep2.CoRootedScopes)
	}
	if !rep2.Connected {
		t.Fatalf("expected Connected==true for the merged scope; gaps: dead=%v unreach=%v cycles=%v stalled=%v corooted=%v",
			rep2.DeadEnds, rep2.UnreachableNodes, rep2.UnboundedCycles, rep2.StalledClosures, rep2.CoRootedScopes)
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
		Subscriber{Name: "in", On: []string{"cli.task"}, In: "global", Emits: []string{"orphan.kind"}},
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
