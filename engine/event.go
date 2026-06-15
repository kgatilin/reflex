// Package engine is the reflex kernel: the three primitives of the settled
// model (docs/24-concept.md §1) — Event, Reaction, Projection — animated by
// the two mechanisms of §4, append and dispatch. Everything else in the
// system is a convention over these three; nothing else is exported.
//
// The invariant, never violated: anything that cannot be recomputed from
// the log is a bug. Session state, scope status, subscription tables,
// budgets — all views over the log, never stores.
package engine

import "encoding/json"

// Event is a record on the append-only log — the first primitive (§1).
// Each of its four parts is one axis with one home (§2): scope lives in
// the subject, correlation in the trace, origin in the payload.
//
// Events are read from the log; they are never constructed by reactions.
// A reaction returns Emit values and the dispatcher does the stamping —
// see Emit.
type Event struct {
	// Subject is {class}.{scope...}.{kind...} in NATS grammar (§2):
	// sys.{kind} for session-less machinery, app.session.{id}.{kind}
	// for session-scoped domain events; every other subject IS the kind of
	// a plain domain event (an externally appended event names its own kind).
	Subject string

	// Trace is the correlation axis, engine-stamped (§2).
	Trace Trace

	// Payload is domain data plus origin ({...data, source}); the engine
	// never branches on it (§4).
	Payload json.RawMessage

	// Meta is opaque, engine-blind side-data attached to an event by its
	// producer — NOT domain payload (a consumer's args) and NOT trace. The
	// engine never reads or branches on it; it only carries it on the log so it
	// round-trips through replay (G8). The motivating use is a reasoning model's
	// per-call thought signature: it must travel WITH the tool-call event (the
	// depth-first walk processes a call's whole subtree before any sibling, so a
	// separate event would land too late) yet stay out of the args the tool
	// consumes. A reaction sets it via Emit.Meta.
	Meta json.RawMessage
}

// There is deliberately no terminal flag (amended 2026-06-12, superseding
// the doc-24 §2 envelope): leaf-ness is a fact of the topology, not a
// sender claim. A kind nobody consumes is a validation lint at Apply; a
// dispatch that reached zero subscribers is an engine fact on the log;
// quiescence is obligation counting; a firing whose emissions open zero
// obligations closes its scope instantly. The envelope carries no
// opinions.

// Trace carries correlation (§2). It is stamped by the dispatcher, never
// written by a reaction: request_id is the narrowest scope covering all
// causes, caused_by is the uprightness rule of doc 17 — membership in a
// scope is a fact of the log, not a sender decision.
type Trace struct {
	SessionID string
	RequestID string
	// SpanID is this event's identity (OTel: event ≡ span, §2).
	SpanID string
	// CausedBy lists the span ids of this event's causes — a list, because
	// join nodes have N causes. caused_by[0] is the OTel parent, the rest
	// are links.
	CausedBy []string
}

// Emit is the only thing a Reaction may produce: kind and payload —
// deliberately nothing else. The dispatcher stamps the trace and places
// the subject scope; a reaction structurally cannot choose its event's
// scope, ancestry, or accounting treatment (§2 uprightness, §5
// membership-is-geometry).
type Emit struct {
	// Kind is the kind tail of the subject; the dispatcher prepends the
	// scope per the subject grammar (§2 handler desugar).
	Kind    string
	Payload json.RawMessage
	// Meta is opaque side-data the dispatcher carries onto the event verbatim
	// (Event.Meta) — engine-blind, never interpreted. It is not scope, ancestry,
	// or accounting (those stay the dispatcher's); it is producer-attached data
	// that must ride with this event (e.g. a model's per-call thought signature).
	Meta json.RawMessage
}
