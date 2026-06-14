package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes"
	"github.com/kgatilin/reflex/pkg/daemon"
	"github.com/kgatilin/reflex/pkg/topology"
)

// emitFactory is a stub body kind for the daemon tests: it emits one fixed kind,
// enough to drive a reconciliation without a real model.
func emitFactory(_ string, config json.RawMessage) (engine.Reaction, error) {
	var c struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(config, &c); err != nil {
		return nil, err
	}
	if c.Kind == "" {
		return nil, fmt.Errorf("emit: no kind")
	}
	return engine.ReactionFunc(func(_ context.Context, _ engine.Event, _ engine.Views) ([]engine.Emit, error) {
		return []engine.Emit{{Kind: c.Kind, Payload: json.RawMessage(`{}`)}}, nil
	}), nil
}

func declarativeDoc() topology.Document {
	return topology.Document{
		Scopes: []topology.ScopeSpec{{Name: "request", Root: "request.received"}},
		Nodes: []topology.NodeSpec{
			{Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global", Emits: []string{"request.received"},
				Body: topology.BodySpec{Kind: "emit", Config: map[string]any{"kind": "request.received"}}},
			{Name: "worker", On: []string{"request.received"}, In: "request", Emits: []string{"task.answered"},
				Body: topology.BodySpec{Kind: "emit", Config: map[string]any{"kind": "task.answered"}}},
			{Name: "notify", On: []string{"task.answered"}, In: "request"},
			{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		},
	}
}

// TestDaemon_ApplyEmitReconciles drives the daemon end-to-end in-process: apply a
// declarative document, emit one ingress with drain, and confirm the
// reconciliation reaches the terminal answer and closes the scope once. This is
// the daemon's Apply/Emit surface working over the descriptor/resolver path.
func TestDaemon_ApplyEmitReconciles(t *testing.T) {
	ctx := context.Background()
	nodes.Register("emit", emitFactory)

	d := daemon.New()
	if err := d.Apply(ctx, declarativeDoc()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	produced, err := d.Emit(ctx, "app.ingress.cli.task", []byte(`{}`), true)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var answered, closed int
	for _, ev := range produced {
		switch engine.KindOf(ev) {
		case "task.answered":
			answered++
		case "scope.request.closed":
			closed++
		}
	}
	if answered != 1 {
		t.Errorf("task.answered = %d, want 1", answered)
	}
	if closed != 1 {
		t.Errorf("scope.request.closed = %d, want 1 (G6)", closed)
	}
}

// TestDaemon_ValidateRejectsDisconnected proves the dry-run surface reports a
// disconnected document without applying it (no events written).
func TestDaemon_ValidateRejectsDisconnected(t *testing.T) {
	nodes.Register("emit", emitFactory)
	d := daemon.New()

	rep, err := d.Validate(topology.Document{
		Nodes: []topology.NodeSpec{
			{Name: "orphan", On: []string{"never.happens"}, In: "global", Emits: []string{"goes.nowhere"},
				Body: topology.BodySpec{Kind: "emit", Config: map[string]any{"kind": "goes.nowhere"}}},
		},
	})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if rep.Connected {
		t.Errorf("disconnected document reported Connected=true")
	}
	if len(d.Events()) != 0 {
		t.Errorf("Validate appended %d events, want 0 (dry-run)", len(d.Events()))
	}
}

// TestDaemon_ApplyRejectsUnknownBodyKind proves a document naming an
// unregistered body kind is rejected by Apply (the resolver fails), leaving the
// live table empty.
func TestDaemon_ApplyRejectsUnknownBodyKind(t *testing.T) {
	ctx := context.Background()
	d := daemon.New()
	err := d.Apply(ctx, topology.Document{
		Scopes: []topology.ScopeSpec{{Name: "request", Root: "request.received"}},
		Nodes: []topology.NodeSpec{
			{Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global", Emits: []string{"request.received"},
				Body: topology.BodySpec{Kind: "nonesuch"}},
			{Name: "sink", On: []string{"request.received"}, In: "request"},
			{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		},
	})
	if err == nil {
		t.Fatalf("Apply with unknown body kind returned nil, want error")
	}
	if n := len(d.Topology()); n != 0 {
		t.Errorf("live topology size = %d, want 0", n)
	}
}
