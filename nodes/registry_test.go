package nodes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes"
	"github.com/kgatilin/reflex/nodes/llm"
)

// emitConfig is a stub body's config: the single kind it emits. The "emit"
// factory below builds a Reaction that emits that kind once — enough to drive a
// reconciliation without a real model, so the descriptor/resolver/Load path is
// tested on its own terms.
type emitConfig struct {
	Kind string `json:"kind"`
}

func emitFactory(_ string, config json.RawMessage) (engine.Reaction, error) {
	var c emitConfig
	if err := json.Unmarshal(config, &c); err != nil {
		return nil, err
	}
	if c.Kind == "" {
		return nil, fmt.Errorf("emit: no kind in config")
	}
	return engine.ReactionFunc(func(_ context.Context, _ engine.Event, _ engine.Views) ([]engine.Emit, error) {
		return []engine.Emit{{Kind: c.Kind, Payload: json.RawMessage(`{}`)}}, nil
	}), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// descriptorTopology is a fully declarative agent shape — every reaction node
// carries a body DESCRIPTOR (kind "emit" + config), no live Go closure. Because
// nothing is a live closure, the whole topology is recoverable from the log via
// the resolver (the point of the test).
func descriptorTopology() []engine.Decl {
	return []engine.Decl{
		engine.Scope{Name: "request", Root: "request.received"},
		engine.Subscriber{
			Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global",
			Emits: []string{"request.received"}, BodyKind: "emit", BodyConfig: mustJSON(emitConfig{Kind: "request.received"}),
		},
		engine.Subscriber{
			Name: "worker", On: []string{"request.received"}, In: "request",
			Emits: []string{"task.answered"}, BodyKind: "emit", BodyConfig: mustJSON(emitConfig{Kind: "task.answered"}),
		},
		engine.Subscriber{Name: "notify", On: []string{"task.answered"}, In: "request"},
		engine.Subscriber{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
	}
}

func collectEvents(e *engine.Engine) []engine.Event {
	var out []engine.Event
	for ev := range e.Events() {
		out = append(out, ev)
	}
	return out
}

func reconciledCounts(e *engine.Engine) (answered, closed int) {
	for ev := range e.Events() {
		switch engine.KindOf(ev) {
		case "task.answered":
			answered++
		case "scope.request.closed":
			closed++
		}
	}
	return
}

// TestDescriptorTopologyRunsAndRecovers proves the body descriptor + resolver
// path (Iteration 2a): a declarative topology applies, runs, and — crucially —
// is RECOVERABLE FROM THE LOG ALONE. A second engine is built with engine.Load
// over the first engine's topology facts (it never sees the Go closures) and
// reconciles the same ingress to the terminal answer. This is G8 for behaviour:
// the body kind + config are facts, the resolver rebuilds the code.
func TestDescriptorTopologyRunsAndRecovers(t *testing.T) {
	ctx := context.Background()
	nodes.Register("emit", emitFactory)
	resolver := engine.WithBodyResolver(nodes.Resolver())

	// First engine: apply the declarative topology (bodies resolved at apply).
	eA := engine.New(resolver)
	if err := eA.Apply(ctx, descriptorTopology()...); err != nil {
		t.Fatalf("Apply (descriptor topology): %v", err)
	}
	topoLog := collectEvents(eA) // the changeset facts only — no domain events yet

	// Second engine: reconstruct from the log via Load. It has the SAME resolver
	// but no in-process bodies handed to it — every body must come from the log.
	eB, err := engine.Load(topoLog, resolver)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := eB.Append(ctx, "app.ingress.cli.task", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := eB.Drain(ctx); err != nil {
		t.Fatalf("Drain (recovered engine): %v", err)
	}

	answered, closed := reconciledCounts(eB)
	if answered != 1 {
		t.Errorf("recovered engine task.answered = %d, want 1 (body rebuilt from the log)", answered)
	}
	if closed != 1 {
		t.Errorf("recovered engine scope.request.closed = %d, want 1 (G6)", closed)
	}
}

// TestApply_UnknownBodyKindRejected proves a descriptor naming an unregistered
// body kind is rejected (not a silent nil body): the resolver fails, Apply
// returns an error, and no node.registered fact is written.
func TestApply_UnknownBodyKindRejected(t *testing.T) {
	ctx := context.Background()
	e := engine.New(engine.WithBodyResolver(nodes.Resolver()))
	err := e.Apply(ctx, engine.Scope{Name: "request", Root: "request.received"},
		engine.Subscriber{
			Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global",
			Emits: []string{"request.received"}, BodyKind: "nonesuch", BodyConfig: json.RawMessage(`{}`),
		},
		engine.Subscriber{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		engine.Subscriber{Name: "sink", On: []string{"request.received"}, In: "request"},
	)
	if err == nil {
		t.Fatalf("Apply with unknown body kind returned nil, want an error")
	}
	if n := len(e.Topology()); n != 0 {
		t.Errorf("live topology size = %d, want 0 (a body that cannot resolve is rejected whole)", n)
	}
}

// TestApply_DescriptorWithNoResolverRejected proves a descriptor node applied to
// an engine with no resolver is a clean configuration error, not a panic.
func TestApply_DescriptorWithNoResolverRejected(t *testing.T) {
	ctx := context.Background()
	e := engine.New() // no resolver
	err := e.Apply(ctx, engine.Scope{Name: "request", Root: "request.received"},
		engine.Subscriber{
			Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global",
			Emits: []string{"request.received"}, BodyKind: "emit", BodyConfig: json.RawMessage(`{}`),
		},
		engine.Subscriber{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		engine.Subscriber{Name: "sink", On: []string{"request.received"}, In: "request"},
	)
	if err == nil {
		t.Fatalf("Apply with a descriptor and no resolver returned nil, want an error")
	}
}

// TestLLMDeclare_ProducesDescriptorAndProjection proves the declarative llm seat
// helper emits a descriptor node (body kind "llm") paired with its llm.history
// projection — the shape the daemon/YAML path applies (the body itself is built
// by llm.Factory through the resolver; not exercised here as it needs a real
// provider).
func TestLLMDeclare_ProducesDescriptorAndProjection(t *testing.T) {
	node, proj := llm.Declare(llm.Config{
		Name: "brain", On: []string{"request.received"}, In: "request",
		Emits: []string{"task.answered", "llm.usage"}, Model: "vertex:google/gemini-2.5-pro",
		System: "You are the brain.",
	})
	if node.BodyKind != "llm" {
		t.Errorf("node.BodyKind = %q, want \"llm\"", node.BodyKind)
	}
	if len(node.BodyConfig) == 0 {
		t.Errorf("node.BodyConfig is empty — the descriptor carries no config")
	}
	if len(node.Reads) != 1 || node.Reads[0] != proj.Name {
		t.Errorf("node.Reads = %v, want [%q] (the paired history projection)", node.Reads, proj.Name)
	}
	if proj.Type != "llm.history" {
		t.Errorf("proj.Type = %q, want \"llm.history\"", proj.Type)
	}
}
