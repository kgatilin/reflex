package llm_test

import (
	"context"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/llm"
	"github.com/kgatilin/reflex/pkg/provider"
)

// emitKinds runs the seat's body once against the scripted provider and returns
// the kinds it emitted, in order.
func emitKinds(t *testing.T, cfg llm.Config, p provider.Provider) []string {
	t.Helper()
	node, _ := llm.NewWithProvider(cfg, p)
	emits, err := node.Body.React(context.Background(), engine.Event{}, schemaViews{})
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	kinds := make([]string, 0, len(emits))
	for _, e := range emits {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func has(ks []string, k string) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// TestContinue_OptInReDrivesOnProseOnlyTurn proves the user's correction: a
// non-tool answer is not a dead end. A seat that lists ContinueKind in its Emits
// re-drives itself (emits llm.continue) when a turn produced no function call —
// whether the model returned prose or nothing — so the loop never stalls at
// quiescence on a chatty turn. A seat that did NOT opt in treats prose as its
// terminal answer and emits no continuation.
func TestContinue_OptInReDrivesOnProseOnlyTurn(t *testing.T) {
	agent := llm.Config{
		Name:  "brain",
		On:    []string{"request.received", llm.ContinueKind},
		Emits: []string{"tool.fs.read.call", "claim.complete", llm.ContinueKind, llm.UsageKind},
		Model: "stub",
	}
	qa := llm.Config{
		Name:  "answerer",
		On:    []string{"request.received"},
		Emits: []string{"task.answered", llm.UsageKind},
		Model: "stub",
	}

	// Prose-only turn: the agent re-drives, the Q&A seat does not.
	proseAgent := emitKinds(t, agent, scripted{text: "I think the fix is to change the operator."})
	if !has(proseAgent, llm.ContinueKind) {
		t.Errorf("agent: prose-only turn did not emit %s; got %v", llm.ContinueKind, proseAgent)
	}
	proseQA := emitKinds(t, qa, scripted{text: "The answer is 42."})
	if has(proseQA, llm.ContinueKind) {
		t.Errorf("q&a seat: prose answer should be terminal, not a continuation; got %v", proseQA)
	}

	// Empty turn (no text, no calls — e.g. a thinking model that ran out of budget
	// before any visible output): the opted-in agent still re-drives (no silent
	// dead end, G4).
	emptyAgent := emitKinds(t, agent, scripted{})
	if !has(emptyAgent, llm.ContinueKind) {
		t.Errorf("agent: empty turn did not emit %s; got %v", llm.ContinueKind, emptyAgent)
	}

	// Actionable turn (a real tool call): NO continuation — the tool result drives
	// the next turn, so re-driving here would fork the loop.
	actedAgent := emitKinds(t, agent, scripted{calls: []string{"tool.fs.read.call"}})
	if has(actedAgent, llm.ContinueKind) {
		t.Errorf("agent: a turn that called a tool must not also continue (fork); got %v", actedAgent)
	}
	// Even a tool call WITH accompanying prose is actionable — the tool advances it.
	actedWithProse := emitKinds(t, agent, scripted{text: "Reading the file now.", calls: []string{"tool.fs.read.call"}})
	if has(actedWithProse, llm.ContinueKind) {
		t.Errorf("agent: tool-call+prose must not continue (fork); got %v", actedWithProse)
	}
}

// TestContinue_NotAdvertisedAsTool proves llm.continue, though in the seat's
// Emits, is never offered to the model as a callable function (it is the seat's
// own bookkeeping, like llm.usage).
func TestContinue_NotAdvertisedAsTool(t *testing.T) {
	cap := &capturing{}
	node, _ := llm.NewWithProvider(llm.Config{
		Name:  "brain",
		On:    []string{"request.received", llm.ContinueKind},
		Emits: []string{"tool.fs.read.call", "claim.complete", llm.ContinueKind, llm.UsageKind},
		Model: "stub",
	}, cap)
	if _, err := node.Body.React(context.Background(), engine.Event{}, schemaViews{}); err != nil {
		t.Fatalf("React: %v", err)
	}
	for _, tl := range cap.last.Tools {
		if tl.Name == llm.ContinueKind || tl.Name == llm.UsageKind {
			t.Errorf("%s advertised as a callable tool; bookkeeping kinds must be excluded. tools=%+v", tl.Name, cap.last.Tools)
		}
	}
	// The real tools ARE advertised.
	var names []string
	for _, tl := range cap.last.Tools {
		names = append(names, tl.Name)
	}
	if !has(names, "tool.fs.read.call") || !has(names, "claim.complete") {
		t.Errorf("expected tool.fs.read.call and claim.complete advertised; got %v", names)
	}
}
