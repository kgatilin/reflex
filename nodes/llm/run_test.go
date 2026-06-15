package llm_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/llm"
	"github.com/kgatilin/reflex/nodes/tool"
	"github.com/kgatilin/reflex/pkg/provider"
)

// scripted is a stub provider for one seat (doc 28 stage 3 acceptance, stub
// provider): each Complete returns a fixed set of function-calls — the seat's
// allowlisted emits — plus token usage. A real model would decide these; the
// stub fixes them so the reconciler runs deterministically.
type scripted struct {
	calls []string // dotted kinds the seat "calls"
	text  string   // optional prose → the answer kind
}

func (s scripted) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	var tcs []provider.ToolCall
	for i, k := range s.calls {
		tcs = append(tcs, provider.ToolCall{ID: strconv.Itoa(i), Name: k, Input: json.RawMessage(`{}`)})
	}
	return provider.Response{
		Text:       s.text,
		ToolCalls:  tcs,
		StopReason: "tool_use",
		Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

// TestRun_Doc27Reconciles wires a doc-27-style topology — resolver → understand
// → fs → gather → plan → execute → notify — with llm seats (scripted stubs) and
// a tool node, applies it (so it passes connectivity validation), feeds one
// external task event, drains, and asserts the request reconciles to task.answered with
// the scope closed and llm.usage logged for every seat (doc 28 stage 3).
func TestRun_Doc27Reconciles(t *testing.T) {
	ctx := context.Background()

	understand, uProj := llm.NewWithProvider(llm.Config{
		Name:   "understand",
		On:     []string{"request.received"},
		In:     "request",
		Emits:  []string{"state.updated.goal", "tool.fs.read.call", "llm.usage"},
		System: "You understand the task.",
	}, scripted{calls: []string{"state.updated.goal", "tool.fs.read.call"}})

	gather, gProj := llm.NewWithProvider(llm.Config{
		Name:   "gather",
		On:     []string{"tool.fs.read.result"},
		In:     "request",
		Emits:  []string{"plan.requested", "llm.usage"},
		System: "You gather context.",
	}, scripted{calls: []string{"plan.requested"}})

	plan, pProj := llm.NewWithProvider(llm.Config{
		Name:   "plan",
		On:     []string{"plan.requested"},
		In:     "request",
		Emits:  []string{"state.updated.plan", "llm.usage"},
		System: "You plan.",
	}, scripted{calls: []string{"state.updated.plan"}})

	execute, eProj := llm.NewWithProvider(llm.Config{
		Name:   "execute",
		On:     []string{"state.updated.plan"},
		In:     "request",
		Emits:  []string{"task.answered", "llm.usage"},
		System: "You execute and answer.",
	}, scripted{text: "done", calls: []string{"task.answered"}})

	// fs tool: tool.fs.read.call → tool.fs.read.result. tool.Node wires the
	// subscription/emits; pin it into the request cone.
	fs := tool.Node("fs.read", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"text":"file contents"}`), nil
	})
	fs.In = "request"

	resolver := engine.Subscriber{
		Name: "resolver",
		// "cli.task" is the external entry kind appended below; subscribing to it
		// both delivers the event at dispatch and makes the resolver a reachability
		// root (a kind no subscriber emits).
		On:    []string{"cli.task"},
		In:    "global",
		Emits: []string{"request.received"},
		Body: engine.ReactionFunc(func(_ context.Context, _ engine.Event, _ engine.Views) ([]engine.Emit, error) {
			return []engine.Emit{{Kind: "request.received", Payload: json.RawMessage(`{"text":"do the task"}`)}}, nil
		}),
	}

	decls := []engine.Decl{
		engine.Scope{Name: "request", Root: "request.received", Budget: map[string]int{"tool.fs.read.call": 16}},
		resolver,
		understand, uProj,
		fs,
		gather, gProj,
		plan, pProj,
		execute, eProj,
		// sinks: the terminal answer, the cost fold, and the closure terminator.
		engine.Subscriber{Name: "notify", On: []string{"task.answered"}, In: "request"},
		engine.Subscriber{Name: "costs", On: []string{"llm.usage"}, In: "request"},
		engine.Subscriber{Name: "lifecycle", On: []string{"scope.request.closed"}},
	}

	e := engine.New()
	if err := e.Apply(ctx, decls...); err != nil {
		rep, _ := engine.Validate(decls...)
		t.Fatalf("Apply: %v\n  DeadEnds=%v\n  Unreachable=%v\n  Fragments=%v\n  UnboundedCycles=%v\n  Stalled=%v\n  DanglingReads=%v\n  UnknownViewTypes=%v",
			err, rep.DeadEnds, rep.UnreachableNodes, rep.Fragments, rep.UnboundedCycles, rep.StalledClosures, rep.DanglingReads, rep.UnknownViewTypes)
	}
	if _, err := e.Append(ctx, "cli.task", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Assert the reconciliation reached its terminal fact, closed the scope, and
	// logged usage per seat.
	var answered, closed, usage int
	for ev := range e.Events() {
		switch engine.KindOf(ev) {
		case "task.answered":
			answered++
		case "scope.request.closed":
			closed++
		case "llm.usage":
			usage++
		}
	}
	if answered != 1 {
		t.Errorf("task.answered count = %d, want 1", answered)
	}
	if closed != 1 {
		t.Errorf("scope.request.closed count = %d, want 1 (closed exactly once, G6)", closed)
	}
	if usage != 4 {
		t.Errorf("llm.usage count = %d, want 4 (one per seat)", usage)
	}
}
