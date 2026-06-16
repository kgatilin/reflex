package engine

import "encoding/json"

// Decl is a managed topology object (doc 20 via §7): subscribers
// (a subscription carries its own wiring), scopes, and projections — the
// four kinds a changeset may create or remove. Declarations pin at the
// instance root; interventions on live instances are ordinary events.
//
// EventKind is also a Decl, but it is not a fourth wiring-kind: it is the
// *bootstrap* form of the event catalog's type axis (doc 26 §4a), a static
// registration of kind→schema that changeset validation can see. The catalog
// is recomputable from EventKind decls + event.registered facts (G8); the decl
// is the seed form, the fact is the runtime form (see catalog.go).
type Decl interface{ isDecl() }

// EventKind registers one catalog entry — a kind paired with its payload
// schema (doc 26 §4a). It is the static/bootstrap form of catalog population,
// visible to Validate so the unknown-kind / dead-subscription checks can run
// over the topology before any event is appended; the runtime form is the
// event.registered fact folded by the same catalog projection.
//
// Schema is JSON Schema as raw bytes, reusing the shape of
// provider.ToolSchema.InputSchema (doc 26 §4: "the payload IS the event's
// type"). It may be nil/empty — a registered kind with no declared schema is
// still a known kind (it makes the kind valid/advertisable), it just imposes
// no payload-conformance constraint at runtime.
type EventKind struct {
	Kind   string
	Schema json.RawMessage
	// Terminal marks a kind as a declared leaf: a graph output (consumed outside
	// the topology, e.g. request.terminal waited on by a client) or an
	// observability fact (llm.usage, an engine scope closure). It is still
	// subscribable, but the validator does NOT lint it as a dead-end / stalled
	// closure when no node consumes it. This is leaf-ness as a TOPOLOGY fact (a
	// catalog declaration), not the per-event sender claim removed on 2026-06-12 —
	// it is the principled alternative to wiring a no-op sink just to satisfy the
	// "every emitted kind needs a consumer" check.
	Terminal bool
}

func (EventKind) isDecl() {}

// Subscriber wires a Reaction into the topology: what it hears, what views it
// reads, what it may emit, and whether its firings root a scope.
type Subscriber struct {
	Name string

	// On lists kind patterns (NATS grammar: * one token, > tail) the node
	// subscribes to. The dispatcher prepends the scope wildcard (§2).
	On []string

	// In optionally scope-qualifies the subscription (§5): events are
	// delivered only inside the named scope's cones.
	In string

	// Reads names the projections attached at dispatch (§6); they arrive
	// as the Views argument of React.
	Reads []string

	// Emits is the declared upper bound on emitted kinds — the static
	// graph and validation read it; exceeding it at runtime is a lint
	// concern, not a dispatch decision (§A.4).
	Emits []string

	// Scope, when non-empty, makes this a scope-rooting node (§5): every
	// firing roots an instance of the named scope. An instance whose
	// emissions open zero obligations closes instantly — the degenerate
	// case is the algebra, not a special case. Closure is announced as
	// scope.{Scope}.closed, exactly once per instance (G6).
	Scope string

	// Body is the node's live Reaction — the in-process form, an arbitrary Go
	// closure. It is NOT serializable, so it is held by name in a process
	// registry (Engine.bodies), never written to the log. In-process callers set
	// it directly.
	Body Reaction

	// BodyKind + BodyConfig are the SERIALIZABLE body descriptor (doc 20 / the
	// daemon path): instead of a live closure, a declarative node names a body
	// kind ("llm", a tool, …) and carries its opaque config. Apply resolves them
	// through the engine's BodyResolver into a Reaction (and caches it in the
	// registry); both ride on the sys.subscriber.registered fact, so a daemon restart
	// rebuilds the body from the log via the resolver (G8). A node sets EITHER a
	// live Body (in-process) OR a BodyKind descriptor (declarative), never both;
	// a node with neither is a sink.
	BodyKind   string
	BodyConfig json.RawMessage
}

func (Subscriber) isDecl() {}

// Scope declares a kind-rooted scope (§5 declared rooting): phases,
// budgets, and the request lifecycle live here. Node-rooted scopes are
// the same concept with the node firing as the root specifier.
type Scope struct {
	Name string
	// Root is the kind whose dispatch roots an instance.
	Root string
	// Budget bounds per-kind counts within one instance's cone (§5
	// "loops are budgets"): a soft scope.budget_low fires one step early,
	// the scope.budget_exhausted hard backstop refuses dispatch (G7).
	Budget map[string]int
	// Detached makes an instance of this scope TOP-LEVEL: it roots a FRESH
	// cone that does NOT inherit its trigger's cones, even when the rooting
	// event is internally caused (doc 31 §4). The causal link stays on the log
	// (caused_by is untouched) — only scope membership detaches, so the
	// sub-computation is a SIBLING of whatever launched it, not a child. This
	// is how a meta-agent dispatches an isolated sub-topology in one runtime:
	// kinds (llm.message, …) are reused freely across detached scopes, and the
	// launcher's own cone is not held open by the work it dispatched
	// (fire-and-forget). A scope rooted by an external event (no cause) is
	// already top-level; Detached generalises that to internally-triggered roots.
	Detached bool
}

func (Scope) isDecl() {}

// Projection declares a fold over a causal horizon (§6) — the third
// primitive. The reference evaluation is a backward walk over caused_by
// from the read position to the horizon root; caches are strategies, the
// walk is the definition (G1 by construction).
type Projection struct {
	Name string
	// On lists the kind patterns folded into the view.
	On []string
	// In is the horizon the backward walk stops at.
	In Horizon
	// Type names the view-type builder that turns the matched events into the
	// view value (doc 26 §4b — generalises the old kv|log Shape into an open
	// registry). "kv" and "log" are built in (their builders are the
	// payload-blind folds below); packages register richer types
	// (e.g. nodes/llm registers "llm.history"). The engine still does only the
	// payload-blind selection — backward walk + On-match → matched events — and
	// the registered builder does the type-specific shaping (RegisterType). An
	// empty Type defaults to "kv" for back-compatibility.
	Type string
	// Key and Value are payload selectors for the kv type, e.g.
	// "payload.path" / "payload.sha". Ignored by the log type; richer types
	// read them as builder params if they wish.
	Key   string
	Value string
	// Params is opaque builder configuration for a richer Type (doc 26 §4b),
	// e.g. nodes/llm encodes the static system base + answer-kind here. The
	// engine never reads it; the registered TypeBuilder decodes it. Ignored by
	// the kv/log built-ins.
	Params json.RawMessage
}

func (Projection) isDecl() {}

// Built-in view types (doc 26 §4b). These are registered names, not a closed
// enum: Type is an open string and packages register more via RegisterType.
const (
	TypeKV  = "kv"
	TypeLog = "log"
)

// Horizon bounds a projection's backward walk (§6).
type Horizon string

const (
	HorizonRequest Horizon = "request"
	// HorizonSession walks the session chain: the resolver chains each
	// request to the previous request's closure, so a session is one
	// connected cone (§6).
	HorizonSession Horizon = "session"
	// HorizonGlobal is log-order, outside any cone — the horizon sys.*
	// behavioural facts and the tool menu need (§B).
	HorizonGlobal Horizon = "global"
)
