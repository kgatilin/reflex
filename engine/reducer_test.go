package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// countReducer is a tiny stateful body (doc 33 §9d): it counts "tick" events and
// emits "counter.two" the moment its count reaches 2. The count is the state the
// ENGINE threads — the reducer itself is pure.
type countReducer struct{}

func (countReducer) Reduce(state any, ev Event) (any, []Emit) {
	n, _ := state.(int)
	if KindOf(ev) == "tick" {
		n++
	}
	if n == 2 {
		return n, []Emit{{Kind: "counter.two", Payload: json.RawMessage(`{}`)}}
	}
	return n, nil
}

// reducerBody adapts a Reducer to a Subscriber.Body: it is a valid Reaction (React
// is never called) AND a Reducer (the engine dispatches it through Reduce).
type reducerBody struct{ r Reducer }

func (reducerBody) React(context.Context, Event, Views) ([]Emit, error) { return nil, nil }
func (b reducerBody) Reduce(s any, ev Event) (any, []Emit)              { return b.r.Reduce(s, ev) }

// TestReducer_ThreadsStatePerScopeInstance proves the engine holds a stateful
// body's state per scope INSTANCE: two independent `s` cones each count their own
// ticks from zero, so each emits counter.two exactly once (the second does NOT
// continue the first's count). This is the view-as-reducer mechanism (doc 33 §9d):
// state in → state + emits out, the body pure, the cache per instance.
func TestReducer_ThreadsStatePerScopeInstance(t *testing.T) {
	ctx := context.Background()
	decls := []Decl{
		Scope{Name: "s", Root: "start", Budget: map[string]int{"tick": 4}},
		// fan: each start emits two ticks into its own s cone.
		Subscriber{Name: "fan", On: []string{"start"}, In: "s", Emits: []string{"tick"}, Body: emitN("tick", 2)},
		// counter: a stateful reducer, one instance of state per s cone.
		Subscriber{Name: "counter", On: []string{"tick"}, In: "s", Emits: []string{"counter.two"},
			Body: reducerBody{r: countReducer{}}},
	}
	e := New()
	e.install(decls...)

	// Two independent task cones (two start roots) in one drain.
	if _, err := e.Append(ctx, "start", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append start#1: %v", err)
	}
	if _, err := e.Append(ctx, "start", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append start#2: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Four ticks total (2 per cone); counter.two fires once PER cone — proving the
	// state is per-instance, not global (a global counter would reach 2 on the
	// second tick and emit only once across both cones).
	if got := countKind(e, "tick"); got != 4 {
		t.Fatalf("tick count = %d, want 4", got)
	}
	if got := countKind(e, "counter.two"); got != 2 {
		t.Fatalf("counter.two count = %d, want 2 (one per scope instance — per-instance state)", got)
	}
}
