package engine

import (
	"context"
	"encoding/json"
)

// Reaction is the second primitive (§1): a pure function
// (event, views) → events. The llm body, tool bodies, projectors — all
// Reactions, no exceptions. Errors are events, not control flow: a
// Reaction error is appended as {node}.failed (non-terminal) into the
// cone and the drain continues (§4 G3); nothing unwinds.
type Reaction interface {
	React(ctx context.Context, ev Event, views Views) ([]Emit, error)
}

// ReactionFunc adapts a plain function to Reaction.
type ReactionFunc func(ctx context.Context, ev Event, views Views) ([]Emit, error)

// React implements Reaction.
func (f ReactionFunc) React(ctx context.Context, ev Event, views Views) ([]Emit, error) {
	return f(ctx, ev, views)
}

// Views is the read surface attached at dispatch (§6): exactly the
// declared projections named in the node's Reads, evaluated at the
// trigger's causal position. "The model had it in context" and "it is in
// the call's causal past" are the same predicate; raw log access does
// not exist for reactions.
type Views interface {
	// Value returns the typed view value by its declared name (doc 26 §4b):
	// the projection's matched events run through its Type's builder, as `any`.
	// A node reads it type-safely through the generic ViewAs helper. An unknown
	// name or unregistered type yields nil (the null object).
	Value(name string) any
	// KV returns a kv-typed view by its declared name — sugar over Value.
	KV(name string) KV
	// Log returns a log-typed view by its declared name: the matched
	// events of the declaration's horizon, in log order — sugar over Value.
	Log(name string) []Event
}

// KV is one of the two projection shapes (§6, deliberately final).
// Ties between incomparable branches break by log order — the single
// writer makes that deterministic.
type KV interface {
	Get(key string) (json.RawMessage, bool)
	Keys() []string
}
