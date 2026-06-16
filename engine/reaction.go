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

// Reducer is a STATEFUL body (doc 33 §9d: view-as-reducer). The engine holds the
// reducer's state per scope instance (a cache rebuildable from the log by replay,
// G8) and threads it through Reduce, so the body itself stays pure: state in →
// state + emits out. A node whose Body also implements Reducer is dispatched
// through Reduce instead of React; its emits are processed identically (catalog
// conformance, depth-first drive). state is nil on the first event of an instance —
// the reducer mints its zero value. The emits are the reducer's CDC: a kind it
// produces when its state crosses a transition (e.g. state.status.waiting),
// bounded by the node's declared Emits allowlist (the graph reads that allowlist,
// never this code — doc 33 §9d).
type Reducer interface {
	Reduce(state any, ev Event) (next any, emits []Emit)
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

	// Schema returns the declared catalog schema for an event kind and whether
	// the kind is in the catalog at all (doc 26 §4a). A body uses it to advertise
	// the parameter schemas of the kinds it may emit — e.g. the llm body turns
	// its Emits + their catalog schemas into function-call schemas. There is no
	// per-tool wiring: the node's Emits allowlist is the menu, the catalog is the
	// schema source, populated dynamically (e.g. by a plugin's announced kinds).
	Schema(kind string) (json.RawMessage, bool)
}

// schemaFunc is the dispatch-time catalog lookup handed to the read surface.
type schemaFunc = func(kind string) (json.RawMessage, bool)

// KV is one of the two projection shapes (§6, deliberately final).
// Ties between incomparable branches break by log order — the single
// writer makes that deterministic.
type KV interface {
	Get(key string) (json.RawMessage, bool)
	Keys() []string
}
