package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// toolNoop mirrors nodes/tool.Node("noop", ...) without importing that package
// (which would import engine → an import cycle in this internal test). It is the
// same shape: subscribed to tool.noop.call, emitting tool.noop.result. The
// dispatch path it exercises is identical to a real tool node.
func toolNoop() Node {
	return Node{
		Name:  "noop",
		On:    []string{"tool.noop.call"},
		Emits: []string{"tool.noop.result", "tool.noop.failed"},
		Body: ReactionFunc(func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
			return []Emit{{Kind: "tool.noop.result", Payload: json.RawMessage(`{"ok":true}`)}}, nil
		}),
	}
}

// emitKind is a tiny deterministic reaction that emits a single fixed kind with
// an empty payload — the plumbing a 2a chain is made of.
func emitKind(kind string) ReactionFunc {
	return func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
		return []Emit{{Kind: kind, Payload: json.RawMessage(`{}`)}}, nil
	}
}

// acyclicTopology builds the spec's chain:
//
//	resolver  On app.ingress.test.msg   → emits request.received
//	echo      On request.received       → emits tool.noop.call
//	noop      (tool)                    → emits tool.noop.result
//	done      On tool.noop.result       → emits task.done   (terminal, no consumer)
func acyclicTopology() []Decl {
	return []Decl{
		Node{
			Name:  "resolver",
			On:    []string{"test.msg"}, // kind tail of app.ingress.test.msg
			Emits: []string{"request.received"},
			Body:  emitKind("request.received"),
		},
		Node{
			Name:  "echo",
			On:    []string{"request.received"},
			Emits: []string{"tool.noop.call"},
			Body:  emitKind("tool.noop.call"),
		},
		toolNoop(),
		Node{
			Name:  "done",
			On:    []string{"tool.noop.result"},
			Emits: []string{"task.done"},
			Body:  emitKind("task.done"),
		},
	}
}

// kindsByOrder returns each event's kind tail in log order.
func kindsByOrder(e *Engine) []string {
	var out []string
	for ev := range e.Events() {
		_, _, kind := splitSubject(ev.Subject)
		out = append(out, kind)
	}
	return out
}

func TestDrain_AcyclicChainFlowsToQuiescence(t *testing.T) {
	ctx := context.Background()

	run := func() *Engine {
		e := New()
		// foldNodes wires In="global" by default, but Apply requires a connected
		// topology; this chain has a terminal (task.done) with no consumer, which
		// Validate flags as a dead-end. The test exercises Drain, not Validate, so
		// store the decls directly.
		e.decls = append(e.decls, acyclicTopology()...)
		if _, err := e.Append(ctx, "app.ingress.test.msg", json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := e.Drain(ctx); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		return e
	}

	e := run()

	kinds := kindsByOrder(e)
	want := []string{"test.msg", "request.received", "tool.noop.call", "tool.noop.result", "task.done"}
	if len(kinds) != len(want) {
		t.Fatalf("log kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("log[%d] kind = %q, want %q (full: %v)", i, kinds[i], want[i], kinds)
		}
	}

	// caused_by chains: the ingress event is uncaused; each emit links to its
	// trigger's span id.
	log := collect(e)
	if len(log[0].Trace.CausedBy) != 0 {
		t.Fatalf("ingress event must be uncaused, got caused_by=%v", log[0].Trace.CausedBy)
	}
	for i := 1; i < len(log); i++ {
		cb := log[i].Trace.CausedBy
		if len(cb) != 1 {
			t.Fatalf("event %d (%s) caused_by = %v, want exactly one cause", i, log[i].Subject, cb)
		}
		if cb[0] != log[i-1].Trace.SpanID {
			t.Fatalf("event %d (%s) caused_by[0] = %q, want trigger span %q",
				i, log[i].Subject, cb[0], log[i-1].Trace.SpanID)
		}
	}

	// session stays empty: the ingress event is pre-resolution, and 2a does not
	// resolve sessions in the chain.
	for _, ev := range log {
		if ev.Trace.SessionID != "" {
			t.Fatalf("event %s has session %q; 2a chain should carry no session", ev.Subject, ev.Trace.SessionID)
		}
	}

	// Determinism: a second identical run yields identical span ids and subjects.
	e2 := run()
	log2 := collect(e2)
	if len(log2) != len(log) {
		t.Fatalf("second run produced %d events, first produced %d", len(log2), len(log))
	}
	for i := range log {
		if log2[i].Trace.SpanID != log[i].Trace.SpanID {
			t.Fatalf("event %d span id not replay-stable: %q vs %q", i, log2[i].Trace.SpanID, log[i].Trace.SpanID)
		}
		if log2[i].Subject != log[i].Subject {
			t.Fatalf("event %d subject not replay-stable: %q vs %q", i, log2[i].Subject, log[i].Subject)
		}
	}
}

func TestDrain_ReactionErrorBecomesFailedEventAndDrainCompletes(t *testing.T) {
	ctx := context.Background()
	e := New()
	e.decls = append(e.decls, Node{
		Name: "boom",
		On:   []string{"test.msg"},
		Body: ReactionFunc(func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
			return nil, errors.New("kaboom")
		}),
	})

	if _, err := e.Append(ctx, "app.ingress.test.msg", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain must complete despite reaction error, got %v", err)
	}

	kinds := kindsByOrder(e)
	want := []string{"test.msg", "boom.failed"}
	if len(kinds) != len(want) || kinds[1] != "boom.failed" {
		t.Fatalf("log kinds = %v, want %v", kinds, want)
	}

	// The failed event carries the error and is caused by the trigger.
	log := collect(e)
	failed := log[1]
	if len(failed.Trace.CausedBy) != 1 || failed.Trace.CausedBy[0] != log[0].Trace.SpanID {
		t.Fatalf("boom.failed caused_by = %v, want [%q]", failed.Trace.CausedBy, log[0].Trace.SpanID)
	}
	var p struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(failed.Payload, &p); err != nil || p.Error != "kaboom" {
		t.Fatalf("boom.failed payload = %s (err %v), want error \"kaboom\"", failed.Payload, err)
	}
}

func collect(e *Engine) []Event {
	var out []Event
	for ev := range e.Events() {
		out = append(out, ev)
	}
	return out
}
