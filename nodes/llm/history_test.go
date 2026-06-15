package llm

import (
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
)

// ev builds a session-scoped event with the given kind tail and payload — the
// subject grammar splitSubject/KindOf expect (app.session.{id}.{kind...}).
func ev(kind, payload string) engine.Event {
	return engine.Event{Subject: "app.session.s1." + kind, Payload: json.RawMessage(payload)}
}

// TestBuildHistory_PositionalSplit locks the doc 26 §4b default split: the
// boundary is the first own-emit; pre-boundary task → first user message, the
// rest → frozen system preamble; post-boundary tail → log-order messages with
// role by Emits-membership.
func TestBuildHistory_PositionalSplit(t *testing.T) {
	params := historyParams{
		System:    "You are an agent.",
		Emits:     []string{"llm.message", "tool.fs.read.call", "plan.requested"},
		Answer:    "llm.message",
		TaskKinds: []string{"request.received"},
	}
	p := engine.Projection{Name: "h", Type: "llm.history", Params: mustMarshal(params)}

	events := []engine.Event{
		ev("request.received", `{"text":"do X"}`),    // task → first user message
		ev("context.found", `{"text":"ctx A"}`),      // preamble → system block
		ev("tool.fs.read.call", `{"path":"f"}`),      // assistant (in Emits) → BOUNDARY
		ev("tool.fs.read.result", `{"text":"data"}`), // tail, not in Emits → user
		ev("llm.message", `{"text":"answer"}`),       // tail, in Emits → assistant
	}

	h, ok := buildHistory(p, events).(History)
	if !ok {
		t.Fatalf("buildHistory did not return a History")
	}

	wantSystem := "You are an agent.\n\n[context.found]\nctx A"
	if h.System() != wantSystem {
		t.Errorf("System() =\n%q\nwant\n%q", h.System(), wantSystem)
	}

	msgs := h.Messages()
	want := []struct{ role, text string }{
		{"user", "do X"},              // the task, pulled out of the preamble
		{"assistant", `{"path":"f"}`}, // the boundary tool-call (no text field → raw JSON)
		{"user", "data"},              // tool result
		{"assistant", "answer"},       // the agent's prose
	}
	if len(msgs) != len(want) {
		t.Fatalf("Messages() len = %d, want %d (%+v)", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Text != w.text {
			t.Errorf("message[%d] = {%q, %q}, want {%q, %q}", i, msgs[i].Role, msgs[i].Text, w.role, w.text)
		}
	}
}

// TestBuildHistory_StructuredToolParts proves the builder reconstructs structured
// function call / response messages from the event log — the function name is the
// kind — and that the result→call pairing is SEAT-AWARE: a result for a call the
// seat itself emits pairs as a ToolResult; a result the seat only observes (a call
// it never made) stays plain text, since an unpaired function response is malformed.
func TestBuildHistory_StructuredToolParts(t *testing.T) {
	params := historyParams{
		System:    "agent",
		Emits:     []string{"llm.message", "tool.fs.read.call"}, // makes fs.read calls; NOT py.test
		Answer:    "llm.message",
		TaskKinds: []string{"request.received"},
	}
	p := engine.Projection{Type: "llm.history", Params: mustMarshal(params)}

	events := []engine.Event{
		ev("request.received", `{"text":"do X"}`),
		ev("tool.fs.read.call", `{"path":"f"}`),         // boundary; the seat's call → ToolCall
		ev("tool.fs.read.result", `{"content":"data"}`), // paired (call ∈ Emits) → ToolResult
		ev("tool.py.test.result", `{"exit":1}`),         // call ∉ Emits → plain text, no ToolResult
		ev("llm.message", `{"text":"done"}`),            // prose → plain assistant text
	}

	h := buildHistory(p, events).(History)
	msgs := h.Messages()
	if len(msgs) != 5 {
		t.Fatalf("Messages() len = %d, want 5 (%+v)", len(msgs), msgs)
	}

	// the call → structured ToolCall, name = the kind.
	if c := msgs[1].ToolCall; c == nil || c.Name != "tool.fs.read.call" || string(c.Input) != `{"path":"f"}` {
		t.Errorf("msgs[1].ToolCall = %+v, want {tool.fs.read.call, {\"path\":\"f\"}}", c)
	}
	// the paired result → structured ToolResult, name = the CALL kind it answers.
	if r := msgs[2].ToolResult; r == nil || r.Name != "tool.fs.read.call" || string(r.Content) != `{"content":"data"}` {
		t.Errorf("msgs[2].ToolResult = %+v, want {tool.fs.read.call, {\"content\":\"data\"}}", r)
	}
	// the observed-only result → NOT paired (seat never emits tool.py.test.call).
	if msgs[3].ToolResult != nil {
		t.Errorf("msgs[3].ToolResult = %+v, want nil (seat does not make py.test calls)", msgs[3].ToolResult)
	}
	if msgs[3].Role != "user" {
		t.Errorf("msgs[3].Role = %q, want user", msgs[3].Role)
	}
	// prose stays plain.
	if msgs[4].ToolCall != nil || msgs[4].Role != "assistant" {
		t.Errorf("msgs[4] = %+v, want plain assistant prose", msgs[4])
	}
}

// TestBuildHistory_NoBoundaryAllPreamble proves that with no own-emit yet, the
// whole cone is preamble: the task is the only message, everything else is
// system (the model has not acted, so nothing is in the tail).
func TestBuildHistory_NoBoundaryAllPreamble(t *testing.T) {
	params := historyParams{
		System:    "base",
		Emits:     []string{"llm.message"},
		TaskKinds: []string{"request.received"},
	}
	p := engine.Projection{Type: "llm.history", Params: mustMarshal(params)}
	events := []engine.Event{
		ev("request.received", `{"text":"task"}`),
		ev("context.found", `{"text":"c"}`),
	}
	h := buildHistory(p, events).(History)
	if got := h.Messages(); len(got) != 1 || got[0].Role != "user" || got[0].Text != "task" {
		t.Errorf("Messages() = %+v, want one user message 'task'", got)
	}
	if got, want := h.System(), "base\n\n[context.found]\nc"; got != want {
		t.Errorf("System() = %q, want %q", got, want)
	}
}
