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

// TestTurnOutcome_ProseIsAClaimToolIsAnAction encodes the live-drive correction:
// a turn has exactly one outcome by what the model DID, and a non-tool answer is
// the model's CLAIM (to be verified), not a self-continuation.
//
//   - prose, no tool  → the answer kind (the claim) — never a "continue" nudge.
//   - empty (opted in)→ the empty kind (a degenerate turn, for a bounded retry).
//   - a tool call     → only the call; incidental prose is dropped (it is an
//     action, not a claim — so it does NOT also emit the answer/claim, which
//     would verify mid-loop).
func TestTurnOutcome_ProseIsAClaimToolIsAnAction(t *testing.T) {
	brain := llm.Config{
		Name:  "brain",
		On:    []string{"request.received", "verify.failed", llm.DefaultEmptyKind},
		Emits: []string{"tool.fs.read.call", llm.DefaultAnswerKind, llm.DefaultEmptyKind, llm.UsageKind},
		Model: "stub",
	}

	// Prose, no tool: the claim (answer kind), no empty.
	prose := emitKinds(t, brain, scripted{text: "I believe the issue is resolved."})
	if !has(prose, llm.DefaultAnswerKind) {
		t.Errorf("prose-only turn did not emit the claim %s; got %v", llm.DefaultAnswerKind, prose)
	}
	if has(prose, llm.DefaultEmptyKind) {
		t.Errorf("prose-only turn must not emit the empty kind; got %v", prose)
	}

	// Empty (opted in): the empty kind, not the claim.
	empty := emitKinds(t, brain, scripted{})
	if !has(empty, llm.DefaultEmptyKind) {
		t.Errorf("empty turn did not emit %s; got %v", llm.DefaultEmptyKind, empty)
	}
	if has(empty, llm.DefaultAnswerKind) {
		t.Errorf("empty turn must not emit the prose claim; got %v", empty)
	}

	// A tool call (even WITH prose): only the call advances — no claim, no empty.
	acted := emitKinds(t, brain, scripted{text: "Reading the file now.", calls: []string{"tool.fs.read.call"}})
	if !has(acted, "tool.fs.read.call") {
		t.Errorf("tool turn did not emit the call; got %v", acted)
	}
	if has(acted, llm.DefaultAnswerKind) || has(acted, llm.DefaultEmptyKind) {
		t.Errorf("a tool turn must not also claim/empty (it is an action); got %v", acted)
	}
}

// TestTurnOutcome_EmptyIsOptIn proves a seat that did not list the empty kind in
// its Emits stays silent on an empty turn (a Q&A seat: prose is its answer, an
// empty turn is just nothing). It still records usage.
func TestTurnOutcome_EmptyIsOptIn(t *testing.T) {
	qa := llm.Config{
		Name:   "answerer",
		On:     []string{"request.received"},
		Emits:  []string{"task.answered", llm.UsageKind},
		Answer: "task.answered",
		Model:  "stub",
	}
	empty := emitKinds(t, qa, scripted{})
	if has(empty, llm.DefaultEmptyKind) {
		t.Errorf("a seat that did not opt into the empty kind emitted it; got %v", empty)
	}
	// only usage on an empty, non-opted-in turn.
	if len(empty) != 1 || empty[0] != llm.UsageKind {
		t.Errorf("empty non-opted-in turn = %v, want just [%s]", empty, llm.UsageKind)
	}
}

// TestTurnOutcome_JudgeSeatFoldsSilenceIntoReject proves the judge pattern: a
// seat whose Answer AND Empty both map to a single "reject" kind, with one
// "approve" function. The model approves only by calling approve; prose OR
// silence both fold to reject (conservative default — a claim is confirmed only
// on an explicit approval). approve is the ONLY advertised tool.
func TestTurnOutcome_JudgeSeatFoldsSilenceIntoReject(t *testing.T) {
	judge := llm.Config{
		Name:   "judge",
		On:     []string{"check.passed"},
		Emits:  []string{"judge.approved", "judge.rejected", llm.UsageKind},
		Answer: "judge.rejected",
		Empty:  "judge.rejected",
		Model:  "stub",
	}

	approve := emitKinds(t, judge, scripted{calls: []string{"judge.approved"}})
	if !has(approve, "judge.approved") || has(approve, "judge.rejected") {
		t.Errorf("explicit approve should emit only judge.approved; got %v", approve)
	}
	rambled := emitKinds(t, judge, scripted{text: "Hmm, the change looks plausible but..."})
	if !has(rambled, "judge.rejected") {
		t.Errorf("prose (no approve call) must fold to judge.rejected; got %v", rambled)
	}
	silent := emitKinds(t, judge, scripted{})
	if !has(silent, "judge.rejected") {
		t.Errorf("silence must fold to judge.rejected; got %v", silent)
	}

	// Only approve is a callable function; reject (== Answer/Empty) is never a tool.
	cap := &capturing{}
	node, _ := llm.NewWithProvider(judge, cap)
	if _, err := node.Body.React(context.Background(), engine.Event{}, schemaViews{}); err != nil {
		t.Fatalf("React: %v", err)
	}
	var names []string
	for _, tl := range cap.last.Tools {
		names = append(names, tl.Name)
	}
	if len(names) != 1 || names[0] != "judge.approved" {
		t.Errorf("judge advertised tools = %v, want only [judge.approved]", names)
	}
}
