package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/engine"
)

// ev builds a session-scoped event with the given kind tail and payload — the
// subject grammar splitSubject/KindOf expect (app.session.{id}.{kind...}).
func ev(kind, payload string) engine.Event {
	return engine.Event{Subject: "app.session.s1." + kind, Payload: json.RawMessage(payload)}
}

// evMeta is ev plus an engine-blind Meta blob (the thought signature rides here).
func evMeta(kind, payload, meta string) engine.Event {
	e := ev(kind, payload)
	e.Meta = json.RawMessage(meta)
	return e
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

// TestBuildHistory_ControlPlaneStructuredMapping proves the explicit calls/results
// config renders a NON-tool control plane as structured function calls + responses
// (not plain text), preserving the thought signature, and synthesizes an ack for a
// fire-and-forget call. This is what the architect's brain needs: its add-* calls
// and the engine's outcomes must be structured, or a thinking model loses continuity.
func TestBuildHistory_ControlPlaneStructuredMapping(t *testing.T) {
	params := historyParams{
		System:    "architect",
		Emits:     []string{"topology.subscriber.add", "task.new"},
		TaskKinds: []string{"task.architect"},
		Calls:     []string{"topology.subscriber.add", "task.new"},
		Results:   []resultPair{{Kind: "topology.changeset.rejected", Answers: "task.new"}},
		AckCalls:  []string{"topology.subscriber.add"}, // silent add → synthetic ack
	}
	p := engine.Projection{Type: "llm.history", Params: mustMarshal(params)}

	events := []engine.Event{
		ev("task.architect", `{"task":"build a worker"}`),
		evMeta("topology.subscriber.add", `{"name":"worker"}`, `{"thought_signature":"c2ln"}`), // call w/ signature
		ev("task.new", `{"task":"build a worker"}`),                                              // dispatch call
		ev("topology.changeset.rejected", `{"reasons":["kind X is a dead-end"]}`),                // its response
	}

	h := buildHistory(p, events).(History)
	msgs := h.Messages()
	// task(user) ; add(assistant call) ; ack(user result) ; task.new(assistant call) ; rejected(user result)
	if len(msgs) != 5 {
		t.Fatalf("Messages() len = %d, want 5: %+v", len(msgs), msgs)
	}
	// the add renders as a structured assistant function call WITH its signature.
	if c := msgs[1].ToolCall; c == nil || c.Name != "topology.subscriber.add" || string(c.Signature) == "" {
		t.Errorf("msgs[1].ToolCall = %+v, want a topology.subscriber.add call with a signature", c)
	}
	// the silent add gets a synthetic ok response (well-formed alternation).
	if r := msgs[2].ToolResult; r == nil || r.Name != "topology.subscriber.add" || string(r.Content) != `{"ok":true}` {
		t.Errorf("msgs[2] = %+v, want a synthetic ack for the silent add", msgs[2])
	}
	// task.new is a structured call; the rejection is its paired function response.
	if c := msgs[3].ToolCall; c == nil || c.Name != "task.new" {
		t.Errorf("msgs[3].ToolCall = %+v, want a task.new call", c)
	}
	if r := msgs[4].ToolResult; r == nil || r.Name != "task.new" {
		t.Errorf("msgs[4].ToolResult = %+v, want the rejection paired to task.new", r)
	}
	if !strings.Contains(msgs[4].Text, "dead-end") {
		t.Errorf("msgs[4].Text = %q, want the rejection reasons readable as text too", msgs[4].Text)
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
