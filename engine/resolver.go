package engine

import "encoding/json"

// BodyResolver builds a node's Reaction from its serializable body descriptor
// (Node.BodyKind + Node.BodyConfig). It is how a topology applied declaratively
// — through YAML, the daemon API, or a replayed log — gets runnable bodies:
// the wiring is a fact on the log, the body kind + config ride on the same fact,
// and the resolver (a registry of factories living ABOVE the kernel, in package
// nodes) turns the descriptor into code. The engine never knows what "llm"
// means; the resolver does. This is the exact parallel of view-type builders
// (RegisterType): behaviour is code resolved by name, the wiring is a fact (G8 —
// the topology, including which body each node uses, is recomputable from the
// log; the resolver rebuilds the code on load).
//
// Returning an error fails the apply: a node whose body cannot be built is a
// rejected changeset, never a silent nil body. The name is passed so a factory
// can stamp it into the body's identity (e.g. the llm node's name).
//
// The whole Subscriber is passed because a body is NOT a pure function of its
// config: the llm body advertises its Emits as the model's function menu (doc 26
// §4), and that wiring lives on the Subscriber, not in the opaque body config.
// Passing the declared node (its Body field still nil — that is what the resolver
// produces) is the deliberate alternative to mirroring Emits into the config in
// the daemon: the body sees the full wiring it depends on, and future bodies may
// read more than Emits without another contract change.
type BodyResolver func(s Subscriber) (Reaction, error)

// Option configures an Engine at construction (New / Load).
type Option func(*Engine)

// WithBodyResolver installs the resolver used to build descriptor-based node
// bodies (see BodyResolver). In-process callers that pass live Body closures on
// their Node decls need no resolver; the daemon and YAML paths require one, and
// Apply rejects a descriptor node when no resolver is installed.
func WithBodyResolver(r BodyResolver) Option { return func(e *Engine) { e.resolver = r } }

// subscriberBodyDescriptor reports whether a node carries a serializable body
// descriptor (a body kind to resolve) rather than a live in-process closure.
func subscriberBodyDescriptor(n Subscriber) bool { return n.Body == nil && n.BodyKind != "" }

var _ = json.Marshal // resolver.go keeps the encoding/json import meaningful for future descriptor helpers
