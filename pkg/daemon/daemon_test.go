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
		Subscribers: []topology.SubscriberSpec{
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

// TestDaemon_LaunchPluginSelfRegisters drives the self-registering-plugin path
// end-to-end: the daemon launches `reflexd plugin echo`, which announces "I
// handle echo.request, I emit echo.reply" (both with schemas). The daemon turns
// that announcement into a GLOBAL subscriber node + two catalog kinds — the
// operator document never mentions the plugin. The operator topology wires who
// emits echo.request (resolver) and who consumes echo.reply (notify); folded with
// the plugin's self-registration it is a connected graph. One ingress then drives
// a reconciliation that reaches the plugin's emit and closes the scope.
func TestDaemon_LaunchPluginSelfRegisters(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the reflexd binary; skipped under -short")
	}
	ctx := context.Background()
	nodes.Register("emit", emitFactory)
	reflexd := buildReflexd(t)

	d := daemon.New()
	defer d.Close()

	// The plugin self-registers its handler (echo.request) + result (echo.reply)
	// and their schemas — no operator plugin node, no host-side catalog wiring.
	name, err := d.LaunchPlugin(ctx, []string{reflexd, "plugin", "echo"})
	if err != nil {
		t.Fatalf("LaunchPlugin: %v", err)
	}
	if name != "echo" {
		t.Fatalf("plugin announced name %q, want echo", name)
	}

	// The operator graph: who EMITS echo.request and who CONSUMES echo.reply —
	// the separate wiring concern. scope.request.closed is registered because a
	// non-empty catalog (grown by the plugin) gates full catalog enforcement and
	// lifecycle subscribes to it.
	doc := topology.Document{
		Scopes: []topology.ScopeSpec{{Name: "request", Root: "echo.request"}},
		Events: []topology.EventSpec{{Kind: "cli.task"}, {Kind: "scope.request.closed"}},
		Subscribers: []topology.SubscriberSpec{
			// app.ingress.* marks the ingress root; cli.task is the kind tail of
			// app.ingress.cli.task, the pattern that actually delivers the ingress.
			{Name: "resolver", On: []string{"app.ingress.*", "cli.task"}, In: "global", Emits: []string{"echo.request"},
				Body: topology.BodySpec{Kind: "emit", Config: map[string]any{"kind": "echo.request"}}},
			{Name: "notify", On: []string{"echo.reply"}, In: "request"},
			{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		},
	}
	if err := d.Apply(ctx, doc); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The catalog gained echo.request and echo.reply from the plugin, recorded as
	// facts (G8): schemas come from the plugin, never hardcoded in the host.
	for _, kind := range []string{"echo.request", "echo.reply"} {
		if !hasEventRegistered(d.Events(), kind) {
			t.Errorf("no sys.event.registered fact for %q — catalog was not populated from the plugin", kind)
		}
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
		t.Errorf("echo.reply = %d, want 1 (the launched plugin handled echo.request)", replied)
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
		Subscribers: []topology.SubscriberSpec{
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
		Subscribers: []topology.SubscriberSpec{
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
