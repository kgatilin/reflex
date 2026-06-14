package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
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

// TestDaemon_ApplyProbesPluginAndPopulatesCatalog drives the dynamic-plugin path
// end-to-end: a plugin node declares NO emits in the document — the daemon spawns
// `reflexd plugin echo`, reads its announced self-description, and fills the
// node's emits AND registers the kind+schema in the catalog. Then one ingress
// drives a reconciliation that reaches the plugin's emit and closes the scope.
func TestDaemon_ApplyProbesPluginAndPopulatesCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the reflexd binary; skipped under -short")
	}
	ctx := context.Background()
	nodes.Register("emit", emitFactory)
	reflexd := buildReflexd(t)

	d := daemon.New()
	defer d.Close()

	doc := topology.Document{
		Scopes: []topology.ScopeSpec{{Name: "request", Root: "request.received"}},
		// Registering the plugin's echo.reply grows the catalog, which flips on
		// full catalog enforcement (validate.go: a non-empty catalog gates
		// Connected) — so every kind must be declared. echo.reply comes from the
		// plugin; the rest are declared here.
		Events: []topology.EventSpec{
			{Kind: "cli.task"},
			{Kind: "request.received"},
			{Kind: "scope.request.closed"},
		},
		Nodes: []topology.NodeSpec{
			{Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global", Emits: []string{"request.received"},
				Body: topology.BodySpec{Kind: "emit", Config: map[string]any{"kind": "request.received"}}},
			// The plugin node: on/in from the document, but NO emits — those come
			// from the plugin's announced self-description (echo.reply).
			{Name: "echoer", On: []string{"request.received"}, In: "request",
				Body: topology.BodySpec{Kind: "plugin", Config: map[string]any{"command": []string{reflexd, "plugin", "echo"}}}},
			{Name: "notify", On: []string{"echo.reply"}, In: "request"},
			{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		},
	}
	if err := d.Apply(ctx, doc); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The catalog gained echo.reply from the plugin, recorded as a fact (G8):
	// schemas come from the plugin, never hardcoded in the host.
	if !hasEventRegistered(d.Events(), "echo.reply") {
		t.Errorf("no sys.event.registered fact for echo.reply — catalog was not populated from the plugin")
	}

	produced, err := d.Emit(ctx, "app.ingress.cli.task", []byte(`{}`), true)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var replied, closed int
	for _, ev := range produced {
		switch engine.KindOf(ev) {
		case "echo.reply":
			replied++
		case "scope.request.closed":
			closed++
		}
	}
	if replied != 1 {
		t.Errorf("echo.reply = %d, want 1 (plugin emitted via the wired-from-spec emit set)", replied)
	}
	if closed != 1 {
		t.Errorf("scope.request.closed = %d, want 1 (G6)", closed)
	}
}

// hasEventRegistered reports whether the log carries a sys.event.registered fact
// for kind.
func hasEventRegistered(log []engine.Event, kind string) bool {
	for _, ev := range log {
		if engine.KindOf(ev) != "event.registered" {
			continue
		}
		var p struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil && p.Kind == kind {
			return true
		}
	}
	return false
}

func buildReflexd(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "reflexd")
	cmd := exec.Command("go", "build", "-o", out, "github.com/kgatilin/reflex/cmd/reflexd")
	cmd.Env = append(cmd.Environ(), "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build reflexd: %v\n%s", err, b)
	}
	return out
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
