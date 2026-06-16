package view

import (
	"encoding/json"
	"strings"

	"github.com/kgatilin/reflex/engine"
)

// The "agent" reducer is the simplest state-machine agent (doc 33 §9): a status
// spine driven by outstanding tool calls. The whole loop — the join, the re-drive,
// the done condition — is this one reducer; the topology wires a library `llm` node
// and tool hands around it. State is the interface; the scope is the hidden
// mechanism (isolation + a budget backstop).
func init() { Register("agent", func() Reducer { return agentReducer{} }) }

// agentState is the per-task cached state the engine holds per task-scope instance.
// Pending counts tool calls emitted but not yet answered; Status is the spine the
// llm triggers on.
type agentState struct {
	Pending int    // outstanding tool calls (emitted, not yet result/failed)
	Status  string // "" | waiting | processing | done
}

// agentReducer is the stateless reducer body — pure logic over the threaded state.
//
// Sequential tool use (one call per turn) keeps the "all answered" predicate exact
// under the engine's depth-first dispatch: a call's result is processed before the
// next call exists, so Pending==0 marks a true turn boundary. A parallel-batch
// gather (Pending over N simultaneous calls) needs obligation counting — a
// turn-scope closure — and is the documented refinement (doc 33 §9c). For a coding
// agent, read → edit → test sequentially is a natural shape anyway.
type agentReducer struct{}

func (agentReducer) Reduce(state any, ev engine.Event) (any, []engine.Emit) {
	st, _ := state.(*agentState)
	if st == nil {
		st = &agentState{}
	}
	prev := st.Status
	kind := engine.KindOf(ev)

	switch {
	case kind == "request.received":
		st.Status = "waiting" // task arrived → the llm's turn
	case isToolCall(kind):
		st.Pending++
		st.Status = "processing"
	case isToolResult(kind):
		if st.Pending > 0 {
			st.Pending--
		}
		if st.Pending == 0 && st.Status != "done" {
			st.Status = "waiting" // all outstanding answered → the llm's turn again
		}
	case kind == "llm.message":
		st.Status = "done" // a no-tool prose turn is the claim of completion
	case kind == "llm.empty":
		// A degenerate (no text, no call) turn: re-prompt. Not a status change, so
		// emit the trigger explicitly; the task scope budget bounds the re-prompts.
		return st, []engine.Emit{statusEmit("waiting")}
	}

	if st.Status != prev && st.Status != "" {
		return st, []engine.Emit{statusEmit(st.Status)}
	}
	return st, nil
}

// statusEmit is the CDC event: the status value rides in the SUBJECT (the kind), so
// a node triggers on a specific value (the llm on state.status.waiting) and dispatch
// stays payload-blind; the value is mirrored in the payload for a reader.
func statusEmit(status string) engine.Emit {
	return engine.Emit{
		Kind:    "state.status." + status,
		Payload: json.RawMessage(`{"status":"` + status + `"}`),
	}
}

func isToolCall(kind string) bool {
	return strings.HasPrefix(kind, "tool.") && strings.HasSuffix(kind, ".call")
}

func isToolResult(kind string) bool {
	return strings.HasPrefix(kind, "tool.") &&
		(strings.HasSuffix(kind, ".result") || strings.HasSuffix(kind, ".failed"))
}
