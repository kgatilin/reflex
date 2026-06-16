package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// connectedTopology is a minimal valid agent shape used by the control-plane
// tests: a resolver on an external entry kind roots the request scope, a worker answers, a notify
// sink consumes the answer, and a lifecycle sink consumes the closure (so no
// dead-end and no stalled closure). It passes the connectivity validator.
func connectedTopology() []Decl {
	return []Decl{
		Scope{Name: "request", Root: "request.received"},
		Subscriber{
			// "cli.task" is the external entry kind appended below; subscribing to
			// it makes the resolver a reachability root (a kind no subscriber emits).
			Name: "resolver", On: []string{"cli.task"}, In: "global", Emits: []string{"request.received"},
			Body: emitKind("request.received"),
		},
		Subscriber{
			Name: "worker", On: []string{"request.received"}, In: "request", Emits: []string{"task.answered"},
			Body: emitKind("task.answered"),
		},
		Subscriber{Name: "notify", On: []string{"task.answered"}, In: "request"},
		Subscriber{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		// Register every domain kind (validation is always on, CONCEPT §6);
		// scope.request.closed is engine-owned, registered automatically.
		EventKind{Kind: "cli.task"}, EventKind{Kind: "request.received"}, EventKind{Kind: "task.answered"},
	}
}

// countSubjects tallies how many log events carry each given subject.
func countSubjects(e *Engine, subjects ...string) map[string]int {
	want := map[string]bool{}
	for _, s := range subjects {
		want[s] = true
	}
	out := map[string]int{}
	for ev := range e.Events() {
		if want[ev.Subject] {
			out[ev.Subject]++
		}
	}
	return out
}

// TestApply_ConnectedChangesetWritesFacts proves the changeset grammar lands on
// the log: a requested fact, one object fact per decl, and an applied fact —
// and that the live table folds back to the applied topology (doc 20 / §8).
func TestApply_ConnectedChangesetWritesFacts(t *testing.T) {
	ctx := context.Background()
	e := New()
	decls := connectedTopology()
	if err := e.Apply(ctx, decls...); err != nil {
		t.Fatalf("Apply (connected): %v", err)
	}

	got := countSubjects(e,
		SubjChangesetRequested, SubjChangesetApplied, SubjChangesetRejected,
		subjScopeDeclared, subjSubscriberRegistered)
	if got[SubjChangesetRequested] != 1 {
		t.Errorf("%s = %d, want 1", SubjChangesetRequested, got[SubjChangesetRequested])
	}
	if got[SubjChangesetApplied] != 1 {
		t.Errorf("%s = %d, want 1", SubjChangesetApplied, got[SubjChangesetApplied])
	}
	if got[SubjChangesetRejected] != 0 {
		t.Errorf("%s = %d, want 0", SubjChangesetRejected, got[SubjChangesetRejected])
	}
	if got[subjScopeDeclared] != 1 {
		t.Errorf("%s = %d, want 1 (one Scope decl)", subjScopeDeclared, got[subjScopeDeclared])
	}
	if got[subjSubscriberRegistered] != 4 {
		t.Errorf("%s = %d, want 4 (four Node decls)", subjSubscriberRegistered, got[subjSubscriberRegistered])
	}

	// The live table folds back to exactly the applied topology, names and all.
	topo := e.Topology()
	gotNames := map[string]bool{}
	for _, d := range topo {
		switch v := d.(type) {
		case Subscriber:
			gotNames["node:"+v.Name] = true
			// Reaction nodes (resolver/worker) must fold back with their Body
			// reattached from the registry; sinks (notify/lifecycle) legitimately
			// have none.
			if (v.Name == "resolver" || v.Name == "worker") && v.Body == nil {
				t.Errorf("node %q folded back with a nil Body — body registry not reattached", v.Name)
			}
		case Scope:
			gotNames["scope:"+v.Name] = true
		}
	}
	for _, want := range []string{"scope:request", "node:resolver", "node:worker", "node:notify", "node:lifecycle"} {
		if !gotNames[want] {
			t.Errorf("live topology missing %q", want)
		}
	}
}

// TestApply_RejectedChangesetWritesNoObjectFacts proves a disconnected changeset
// is recorded as rejected with NO object facts, leaving the live table unchanged
// by construction (doc 20: "the live table folds only facts", and a rejection
// writes none).
func TestApply_RejectedChangesetWritesNoObjectFacts(t *testing.T) {
	ctx := context.Background()
	e := New()

	// A worker with no external root and a dead-end answer — disconnected.
	bad := []Decl{
		Subscriber{Name: "orphan", On: []string{"never.happens"}, In: "global", Emits: []string{"goes.nowhere"}, Body: emitKind("goes.nowhere")},
	}
	err := e.Apply(ctx, bad...)
	if err == nil {
		t.Fatalf("Apply (disconnected) returned nil, want ValidationError")
	}
	if _, ok := err.(*ValidationError); !ok {
		t.Fatalf("Apply error type = %T, want *ValidationError", err)
	}

	got := countSubjects(e, SubjChangesetRequested, SubjChangesetRejected, SubjChangesetApplied, subjSubscriberRegistered)
	if got[SubjChangesetRequested] != 1 {
		t.Errorf("%s = %d, want 1 (the request is recorded even on rejection)", SubjChangesetRequested, got[SubjChangesetRequested])
	}
	if got[SubjChangesetRejected] != 1 {
		t.Errorf("%s = %d, want 1", SubjChangesetRejected, got[SubjChangesetRejected])
	}
	if got[SubjChangesetApplied] != 0 {
		t.Errorf("%s = %d, want 0", SubjChangesetApplied, got[SubjChangesetApplied])
	}
	if got[subjSubscriberRegistered] != 0 {
		t.Errorf("%s = %d, want 0 (a rejected changeset writes no object facts)", subjSubscriberRegistered, got[subjSubscriberRegistered])
	}
	if n := len(e.Topology()); n != 0 {
		t.Errorf("live topology size = %d, want 0 (rejection left the table unchanged)", n)
	}
}

// TestApply_LiveTableIsAFoldOfTheLog proves G8 for the topology: a second engine
// fed ONLY the recorded log facts (plus the body registry, which is code, not a
// fact) reconstructs the identical wiring. The live table is recomputable from
// the log — not a privileged store.
func TestApply_LiveTableIsAFoldOfTheLog(t *testing.T) {
	ctx := context.Background()
	e := New()
	if err := e.Apply(ctx, connectedTopology()...); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Refold the recorded log with the same bodies: the wiring must match the
	// engine's live cache exactly (the cache is a memoisation of this fold).
	refold := foldTopology(append([]Event(nil), e.log...), e.bodies)
	if len(refold) != len(e.Topology()) {
		t.Fatalf("refold size = %d, live size = %d — the cache is not the fold of the log", len(refold), len(e.Topology()))
	}
	gotNodes, gotScopes := 0, 0
	for _, d := range refold {
		switch d.(type) {
		case Subscriber:
			gotNodes++
		case Scope:
			gotScopes++
		}
	}
	if gotNodes != 4 || gotScopes != 1 {
		t.Errorf("refold = %d nodes, %d scopes; want 4 nodes, 1 scope", gotNodes, gotScopes)
	}
}

// TestApply_ChangesetPipelineDrivesARun proves the topology applied through the
// changeset pipeline actually dispatches: one external event reconciles to the
// terminal answer and the scope closes exactly once (G6). This is the control
// plane and the dispatcher working together end-to-end.
func TestApply_ChangesetPipelineDrivesARun(t *testing.T) {
	ctx := context.Background()
	e := New()
	if err := e.Apply(ctx, connectedTopology()...); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := e.Append(ctx, "cli.task", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	var answered, closed int
	for ev := range e.Events() {
		switch KindOf(ev) {
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

// TestSelfMutationReasons covers the foreign-scope rule (doc 31 §4): a changeset
// issued from inside a scope may compose OTHER scopes but not mutate the cone it
// runs in. The pure check is exercised across every decl kind.
func TestSelfMutationReasons(t *testing.T) {
	issuing := map[string]struct{}{"architect": {}}

	cases := []struct {
		name    string
		decls   []Decl
		flagged bool
	}{
		{"empty issuing never flags", nil /*set below*/, false},
		{"subscriber in own scope is flagged",
			[]Decl{Subscriber{Name: "intruder", In: "architect"}}, true},
		{"subscriber in foreign scope is fine",
			[]Decl{Subscriber{Name: "worker", In: "request"}}, false},
		{"subscriber in global is fine",
			[]Decl{Subscriber{Name: "g", In: "global"}}, false},
		{"subscriber with empty scope (global) is fine",
			[]Decl{Subscriber{Name: "g", In: ""}}, false},
		{"redeclaring own scope is flagged",
			[]Decl{Scope{Name: "architect", Root: "task.architect"}}, true},
		{"declaring a foreign scope is fine",
			[]Decl{Scope{Name: "request", Root: "request.received"}}, false},
		{"projection in own scope is flagged",
			[]Decl{Projection{Name: "spy", In: "architect"}}, true},
		{"event decl is catalog-global, never flagged",
			[]Decl{EventKind{Kind: "x.k"}}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			iss := issuing
			if c.name == "empty issuing never flags" {
				iss = nil
				c.decls = []Decl{Subscriber{Name: "intruder", In: "architect"}}
			}
			got := selfMutationReasons(iss, c.decls)
			if c.flagged && len(got) == 0 {
				t.Fatalf("expected a foreign-scope violation, got none")
			}
			if !c.flagged && len(got) != 0 {
				t.Fatalf("expected no violation, got %v", got)
			}
		})
	}
}
