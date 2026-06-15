package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// TestInGraphChangeset_AppliesAndLiveRefreshes is the core of the in-graph
// control plane (doc 20): a node DRIVES a topology changeset by emitting
// KindChangesetRequested as an ordinary event, and a SIBLING emit in the SAME
// turn is handled by the subgraph the changeset just added — the mid-drain
// live-refresh. Without the refresh, "go2" would be dispatched against the node
// set captured at Drain entry (no worker), and "done" would never be produced.
//
// The architect fires once on the external "go", returning two emits in order:
//  1. a changeset that ADDS a "worker" node (On go2 → emits done), then
//  2. "go2" itself.
//
// Depth-first dispatch drives emit 1 to completion (the engine applies the
// changeset and refreshes the live node set) before emit 2 is appended, so by
// the time "go2" is processed the worker is live and fires — all in one Drain.
func TestInGraphChangeset_AppliesAndLiveRefreshes(t *testing.T) {
	ctx := context.Background()

	// The worker body is a descriptor resolved by the engine's resolver — exactly
	// how a changeset-added node (whose Body cannot ride as a closure through ops)
	// gets its code. "emit-done" → a reaction emitting "done".
	res := func(s Subscriber) (Reaction, error) {
		if s.BodyKind == "emit-done" {
			return emitKind("done"), nil
		}
		return nil, fmt.Errorf("unknown body kind %q", s.BodyKind)
	}
	e := New(WithBodyResolver(res))

	// The worker the architect will graft in. Its Body is a descriptor (BodyKind),
	// not a closure, so it survives the ops round-trip.
	worker := Subscriber{Name: "worker", On: []string{"go2"}, Emits: []string{"done"}, BodyKind: "emit-done"}

	architect := []Decl{
		EventKind{Kind: "go"},                   // external entry (root)
		EventKind{Kind: "go2", Terminal: true},  // architect emits it; consumer grafted at runtime
		EventKind{Kind: "done", Terminal: true}, // the worker's output
		Subscriber{
			Name:  "architect",
			On:    []string{"go"},
			Emits: []string{KindChangesetRequested, "go2"},
			Body: ReactionFunc(func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
				return []Emit{
					{Kind: KindChangesetRequested, Payload: ChangesetRequestPayload([]Decl{worker}, "architect")},
					{Kind: "go2", Payload: json.RawMessage(`{}`)},
				}, nil
			}),
		},
	}
	if err := e.Apply(ctx, architect...); err != nil {
		t.Fatalf("Apply architect: %v", err)
	}

	// The worker is NOT live yet — only the architect is.
	if got := len(e.liveSubscribers()); got != 1 {
		t.Fatalf("live subscribers before run = %d, want 1 (architect only)", got)
	}

	if _, err := e.Append(ctx, "go", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append go: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	var requested, applied, rejected, done int
	var reqSpan, appliedCause string
	for ev := range e.Events() {
		switch KindOf(ev) {
		case KindChangesetRequested:
			requested++
			reqSpan = ev.Trace.SpanID
		case KindChangesetApplied:
			applied++
			if len(ev.Trace.CausedBy) > 0 {
				appliedCause = ev.Trace.CausedBy[0]
			}
		case KindChangesetRejected:
			rejected++
		case "done":
			done++
		}
	}

	// Two changeset pairs occur: the bootstrap operator Apply of the architect
	// topology (requested with an empty cause), and the architect's in-graph
	// changeset (requested caused by the external "go"). Both share the kind tail.
	if requested != 2 {
		t.Errorf("changeset.requested count = %d, want 2 (bootstrap Apply + in-graph)", requested)
	}
	if applied != 2 {
		t.Errorf("changeset.applied count = %d, want 2 (bootstrap Apply + in-graph)", applied)
	}
	if rejected != 0 {
		t.Errorf("changeset.rejected count = %d, want 0", rejected)
	}
	// reqSpan/appliedCause hold the LAST of each (the in-graph pair): the in-graph
	// applied hangs under the in-graph request span.
	if appliedCause != reqSpan {
		t.Errorf("in-graph applied caused_by = %q, want the in-graph request span %q", appliedCause, reqSpan)
	}
	// The payload of the whole experiment: the grafted worker ran in the SAME
	// drain, on the sibling emit, because the live node set refreshed mid-drain.
	if done != 1 {
		t.Errorf("done count = %d, want 1 (the grafted worker fired on the sibling emit)", done)
	}
	// And it is genuinely live now.
	if got := len(e.liveSubscribers()); got != 2 {
		t.Errorf("live subscribers after run = %d, want 2 (architect + worker)", got)
	}
}

// TestInGraphChangeset_RejectsDisconnected proves a node-emitted changeset that
// would disconnect the graph is rejected in-graph (a rejected fact on the log,
// no worker added) rather than applied — the same connectivity gate an operator
// Apply faces, enforced on the in-graph path.
func TestInGraphChangeset_RejectsDisconnected(t *testing.T) {
	ctx := context.Background()
	res := func(s Subscriber) (Reaction, error) { return emitKind("nowhere"), nil }
	e := New(WithBodyResolver(res))

	// island: subscribes to a kind nobody produces AND emits a kind nobody
	// consumes and that is not registered → a disconnected fragment + unknown
	// kind. The changeset must be rejected.
	island := Subscriber{Name: "island", On: []string{"unreachable.kind"}, Emits: []string{"nowhere"}, BodyKind: "x"}

	architect := []Decl{
		EventKind{Kind: "go"},
		Subscriber{
			Name:  "architect",
			On:    []string{"go"},
			Emits: []string{KindChangesetRequested},
			Body: ReactionFunc(func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
				return []Emit{{Kind: KindChangesetRequested, Payload: ChangesetRequestPayload([]Decl{island}, "architect")}}, nil
			}),
		},
	}
	if err := e.Apply(ctx, architect...); err != nil {
		t.Fatalf("Apply architect: %v", err)
	}
	if _, err := e.Append(ctx, "go", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	var applied, rejected int
	for ev := range e.Events() {
		switch KindOf(ev) {
		case KindChangesetApplied:
			applied++
		case KindChangesetRejected:
			rejected++
		}
	}
	if rejected != 1 {
		t.Errorf("changeset.rejected = %d, want 1 (disconnected fragment)", rejected)
	}
	// One applied: the bootstrap operator Apply of the architect topology. The
	// in-graph island changeset is rejected, not applied.
	if applied != 1 {
		t.Errorf("changeset.applied = %d, want 1 (bootstrap Apply only)", applied)
	}
	if got := len(e.liveSubscribers()); got != 1 {
		t.Errorf("live subscribers = %d, want 1 (island not grafted)", got)
	}
}
