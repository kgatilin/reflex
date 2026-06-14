package engine

// Decl is a managed topology object (doc 20 via §7): nodes,
// subscriptions (carried on the node), scopes, and projections — the
// four kinds a changeset may create or remove. Declarations pin at the
// instance root; interventions on live instances are ordinary events.
type Decl interface{ isDecl() }

// Kind tags a node by the nature of its body (doc 27 §6): the connectivity
// validator reads it to decide which nodes may serve as the deterministic
// guard of a cycle (KindDeterministic) and which are bridges that may not be
// relied on to enforce a loop exit (KindLLM). The zero value is
// KindDeterministic — a node with no declared kind is treated as plain
// deterministic machinery, the conservative default for a guard candidate.
type Kind string

const (
	// KindDeterministic is plain machinery: tool dispatch is not modelled by
	// this kind (tools carry KindTool); deterministic covers guards,
	// resolvers, and any pure transition that is not an LLM bridge. It is the
	// zero value, so an unset Node.Kind reads as deterministic.
	KindDeterministic Kind = "deterministic"
	// KindLLM marks a bridge node whose body is a language model (doc 27 §1):
	// LLM nodes sit at dead-ends and emit the entry kinds of otherwise
	// disconnected fragments. They may not be counted as a cycle's guard.
	KindLLM Kind = "llm"
	// KindTool marks a tool-dispatch node (nodes/tool): subscribed to
	// tool.{name}.call, emitting tool.{name}.result / .failed.
	KindTool Kind = "tool"
)

// orDefault returns the node's kind, defaulting the zero value to
// KindDeterministic so no node is ever kind-less.
func (k Kind) orDefault() Kind {
	if k == "" {
		return KindDeterministic
	}
	return k
}

// Node wires a Reaction into the topology: what it hears, what views it
// reads, what it may emit, and whether its firings root a scope.
type Node struct {
	Name string

	// Kind tags the body's nature (llm / tool / deterministic) for the
	// connectivity validator (doc 27 §6). Empty defaults to deterministic.
	Kind Kind

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

	Body Reaction
}

func (Node) isDecl() {}

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
	// Shape is kv or log — deliberately final (§6); anything richer is a
	// reaction emitting state.updated.{path} facts served by a generic kv.
	Shape Shape
	// Key and Value are payload selectors for the kv shape, e.g.
	// "payload.path" / "payload.sha". Ignored for the log shape.
	Key   string
	Value string
}

func (Projection) isDecl() {}

// Shape is a projection's view shape (§6): two only.
type Shape string

const (
	ShapeKV  Shape = "kv"
	ShapeLog Shape = "log"
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
