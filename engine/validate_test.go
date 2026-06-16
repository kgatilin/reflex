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
		!anyContains(rep.Suggestions, "is a dead-end") {
		t.Fatalf("expected a dead-end suggestion for task.answered; got %v", rep.Suggestions)
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

func TestValidate_ScopeClosureNeedsNoConsumer(t *testing.T) {
	// A scope closure (scope.X.closed / .budget_exhausted) is an engine
	// observability leaf — marked terminal in the catalog. A topology need NOT
	// consume it: there is no no-op "lifecycle" sink to satisfy the validator.
	// The loop is bounded by the scope budget; the closure is a trace fact, not a
	// dead-end. (Termination as a guarantee is the operator's job — wire the
	// scope's resolution to a real output — not a forced scope.closed consumer.)
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
	if len(rep.StalledClosures) != 0 {
		t.Fatalf("a scope closure is terminal — never a stalled closure; got %v", rep.StalledClosures)
	}
	if !rep.Connected {
		t.Fatalf("expected Connected==true without a scope.closed consumer; gaps: dead=%v unreach=%v cycles=%v stalled=%v",
			rep.DeadEnds, rep.UnreachableNodes, rep.UnboundedCycles, rep.StalledClosures)
	}

	// It is still SUBSCRIBABLE — consuming it (e.g. an audit terminator) is fine too.
	withConsumer := append(decls, Subscriber{Name: "audit", On: []string{"scope.work.closed"}, In: "global"})
	rep2, err := Validate(withConsumer...)
	if err != nil {
		t.Fatalf("Validate (with consumer): %v", err)
	}
	if !rep2.Connected {
		t.Fatalf("expected Connected==true with a scope.closed consumer too; gaps: dead=%v unreach=%v cycles=%v stalled=%v",
			rep2.DeadEnds, rep2.UnreachableNodes, rep2.UnboundedCycles, rep2.StalledClosures)
	}
}

// TestValidate_TerminalKindNeedsNoConsumer proves an operator-declared terminal
// kind (a graph OUTPUT, e.g. request.terminal waited on by a client) is not a
// dead-end even though no node consumes it.
func TestValidate_TerminalKindNeedsNoConsumer(t *testing.T) {
	decls := []Decl{
		Subscriber{Name: "resolver", On: []string{"cli.task"}, In: "global", Emits: []string{"request.terminal"}},
		EventKind{Kind: "cli.task"},
		EventKind{Kind: "request.terminal", Terminal: true},
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if contains(rep.DeadEnds, "request.terminal") {
		t.Fatalf("a terminal output must not be a dead-end; got %v", rep.DeadEnds)
	}
	if !rep.Connected {
		t.Fatalf("expected Connected==true with a terminal output and no consumer; got dead=%v", rep.DeadEnds)
	}

	// Without the terminal marker, the same kind IS a dead-end (the default).
	rep2, err := Validate(
		Subscriber{Name: "resolver", On: []string{"cli.task"}, In: "global", Emits: []string{"goes.nowhere"}},
		EventKind{Kind: "cli.task"}, EventKind{Kind: "goes.nowhere"},
	)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !contains(rep2.DeadEnds, "goes.nowhere") {
		t.Fatalf("an unmarked unconsumed kind should be a dead-end; got %v", rep2.DeadEnds)
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

// TestValidate_DetachedScopeSuppressesCrossScopeCycle is the doc 31 §4 / increment
// 2 proof: a meta-agent and the sub-topology it dispatches live in ONE validated
// document, REUSE the same turn kind (llm.message), and the meta-agent subscribes
// to that kind — which, scope-blind, draws a phantom cross-scope cycle through the
// global injector:
//
//	meta-brain  On [task.meta, llm.message]      in meta    → task.new
//	resolver    On [task.new]                    in global  → request.received  (PORT)
//	worker      On [request.received, llm.message] in worker → llm.message       (REUSED)
//
// SCC {meta-brain, resolver, worker} runs through `resolver` (global, unbudgeted),
// so a scope-blind validator reports it unbounded and REJECTS. With the worker
// scope DETACHED it is top-level — a sibling of the externally-rooted meta scope —
// so the worker→meta-brain edge on llm.message is a phantom the validator no longer
// draws (deliverableStatic), leaving only the worker's own budgeted self-loop. No
// cut pass, no per-component split: plain whole-graph SCC on a scope-aware edge set.
func TestValidate_DetachedScopeSuppressesCrossScopeCycle(t *testing.T) {
	build := func(detached bool) []Decl {
		return []Decl{
			// meta: externally-rooted (task.meta is produced by no node) → top-level.
			Scope{Name: "meta", Root: "task.meta", Budget: map[string]int{"task.new": 4}},
			// worker: rooted by request.received, which resolver PRODUCES — so it is
			// top-level ONLY when declared Detached. That toggle is the whole test.
			Scope{Name: "worker", Root: "request.received", Budget: map[string]int{"llm.message": 8}, Detached: detached},
			Subscriber{Name: "meta-brain", On: []string{"task.meta", "llm.message"}, In: "meta", Emits: []string{"task.new"}},
			Subscriber{Name: "resolver", On: []string{"task.new"}, In: "global", Emits: []string{"request.received"}},
			Subscriber{Name: "worker", On: []string{"request.received", "llm.message"}, In: "worker", Emits: []string{"llm.message"}},
			EventKind{Kind: "task.meta"}, EventKind{Kind: "task.new"},
			EventKind{Kind: "request.received"}, EventKind{Kind: "llm.message"},
		}
	}

	t.Run("detached worker is isolated — no cross-scope cycle", func(t *testing.T) {
		rep, err := Validate(build(true)...)
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if len(rep.UnboundedCycles) != 0 {
			t.Fatalf("a detached worker must not form a cross-scope unbounded cycle; got %v", rep.UnboundedCycles)
		}
		if !rep.Connected {
			t.Fatalf("expected Connected==true with a detached worker; gaps: dead=%v unreach=%v cycles=%v stalled=%v",
				rep.DeadEnds, rep.UnreachableNodes, rep.UnboundedCycles, rep.StalledClosures)
		}
	})

	t.Run("nested control leaks the phantom cycle", func(t *testing.T) {
		rep, err := Validate(build(false)...)
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		// Scope-blind, the worker's llm.message reaches meta-brain → the cross-scope
		// SCC through the global resolver is reported unbounded.
		found := false
		for _, scc := range rep.UnboundedCycles {
			if contains(scc, "meta-brain") && contains(scc, "worker") && contains(scc, "resolver") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the cross-scope SCC {meta-brain,resolver,worker} reported unbounded without detachment; got %v", rep.UnboundedCycles)
		}
	})
}

// TestValidate_DidYouMeanForUnknownKinds proves the validator steers a misspelled
// or invented kind to the real one (doc 27 §5): separator confusion and small
// typos get a "did you mean" hit, and a tool-shaped name additionally lists the
// real tool-call kinds — the feedback a meta-agent needs to fix a hallucinated kind.
func TestValidate_DidYouMeanForUnknownKinds(t *testing.T) {
	// A catalog with the real host tool kinds + an external entry.
	catalog := []Decl{
		EventKind{Kind: "request.received"},
		EventKind{Kind: "tool.fs.read.call"}, EventKind{Kind: "tool.fs.read.result"},
		EventKind{Kind: "tool.fs.edit.call"}, EventKind{Kind: "tool.py.test.call"},
	}

	t.Run("separator confusion is corrected", func(t *testing.T) {
		decls := append([]Decl{
			Subscriber{Name: "w", On: []string{"request.received"}, In: "global", Emits: []string{"tool_fs_read_call"}},
		}, catalog...)
		rep, _ := Validate(decls...)
		if !anyContains(rep.Suggestions, `did you mean "tool.fs.read.call"`) {
			t.Fatalf("expected a did-you-mean for tool_fs_read_call; got %v", rep.Suggestions)
		}
	})

	t.Run("typo is corrected", func(t *testing.T) {
		decls := append([]Decl{
			Subscriber{Name: "w", On: []string{"request.received"}, In: "global", Emits: []string{"tool.fs.read.return"}},
		}, catalog...)
		rep, _ := Validate(decls...)
		if !anyContains(rep.Suggestions, `did you mean "tool.fs.read.result"`) {
			t.Fatalf("expected a did-you-mean for tool.fs.read.return; got %v", rep.Suggestions)
		}
	})

	t.Run("invented tool lists the real tool kinds", func(t *testing.T) {
		decls := append([]Decl{
			Subscriber{Name: "w", On: []string{"request.received"}, In: "global", Emits: []string{"tool.bash.call"}},
		}, catalog...)
		rep, _ := Validate(decls...)
		if !anyContains(rep.Suggestions, "available tool kinds are") ||
			!anyContains(rep.Suggestions, "tool.fs.read.call") {
			t.Fatalf("expected the available tool kinds listed for tool.bash.call; got %v", rep.Suggestions)
		}
	})
}

// TestValidate_DeadEndGuidanceNamesCycleFreeConsumer proves a dead-end suggestion
// names the existing node(s) that could consume the kind WITHOUT forming an
// unbounded cycle (doc 31 §5) — concrete wiring, not a generic "add an llm node".
// And when no such node exists, it falls back to "register it terminal".
func TestValidate_DeadEndGuidanceNamesCycleFreeConsumer(t *testing.T) {
	t.Run("names an acyclic consumer in the draft", func(t *testing.T) {
		decls := []Decl{
			Scope{Name: "worker", Root: "request.received", Budget: map[string]int{"llm.message": 5}},
			// agent emits orphan.kind (a dead-end) plus llm.message.
			Subscriber{Name: "agent", In: "worker", On: []string{"request.received"},
				Emits: []string{"llm.message", "orphan.kind"}, BodyKind: "llm"},
			// collector consumes llm.message and only emits a terminal — it cannot
			// reach agent, so wiring orphan.kind into it is acyclic.
			Subscriber{Name: "collector", In: "worker", On: []string{"llm.message"},
				Emits: []string{"request.terminal"}, BodyKind: "entry"},
			EventKind{Kind: "request.received"},
			EventKind{Kind: "llm.message"},
			EventKind{Kind: "orphan.kind"},
			EventKind{Kind: "request.terminal", Terminal: true},
		}
		rep, _ := Validate(decls...)
		if !anyContains(rep.Suggestions, `"orphan.kind"`) ||
			!anyContains(rep.Suggestions, "collector") ||
			!anyContains(rep.Suggestions, "without forming an unbounded cycle") {
			t.Fatalf("expected orphan.kind guidance to name collector as a cycle-free consumer; got %v", rep.Suggestions)
		}
	})

	t.Run("falls back to terminal when no acyclic consumer exists", func(t *testing.T) {
		// A lone unbudgeted node emitting a leaf: the only possible consumer is
		// itself (a self-loop), which is unbounded — so no candidate, and the
		// guidance must steer to registering the kind terminal.
		decls := []Decl{
			Subscriber{Name: "agent", In: "global", On: []string{"request.received"},
				Emits: []string{"llm.message"}, BodyKind: "llm"},
			EventKind{Kind: "request.received"},
			EventKind{Kind: "llm.message"},
		}
		rep, _ := Validate(decls...)
		if !anyContains(rep.Suggestions, `"llm.message"`) ||
			!anyContains(rep.Suggestions, "register it terminal") ||
			!anyContains(rep.Suggestions, "no existing node") {
			t.Fatalf("expected llm.message guidance to steer to terminal; got %v", rep.Suggestions)
		}
	})
}
