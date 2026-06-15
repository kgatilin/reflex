// Package nodes is the body-factory registry that sits ABOVE the kernel (doc 20
// / CONCEPT §8, §12): it maps a node's declared body kind ("llm", a tool, …) to
// a factory that builds the runnable engine.Reaction from the node's serialized
// config. The engine stays body-agnostic — it carries the body descriptor
// (kind + config) on the sys.subscriber.registered fact and asks an injected
// engine.BodyResolver to turn it into code. This package IS that resolver: the
// composition root (a daemon, a test) registers the factories it wants
// (nodes.Register("llm", llm.Factory)) and hands engine.WithBodyResolver(
// nodes.Resolver()) to the engine, so a declarative or replayed topology gets
// runnable bodies. Behaviour is code resolved by name; wiring is a fact on the
// log (G8) — the same split as view-type builders.
package nodes

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kgatilin/reflex/engine"
)

// Factory builds a node's Reaction from its declared Subscriber (name, emit
// allowlist, body config, …). It is the per-kind half of an engine.BodyResolver.
// The whole Subscriber is passed because some bodies (llm) derive their
// behaviour — the model's function menu — from the subscriber's Emits, which is
// wiring, not config; a factory that needs only the config reads s.BodyConfig.
type Factory func(s engine.Subscriber) (engine.Reaction, error)

// Describer optionally augments a Factory: it declares the catalog kinds a body
// CONTRIBUTES for a given subscriber — the kind(s) it HANDLES (with the schema a
// reasoning node advertises as that tool's parameter schema) and the kind(s) it
// EMITS. It is the in-process parallel of a plugin's hello self-description: a
// body that defines a tool registers that tool's schema WITH ITSELF, so an
// operator topology wires the body without re-declaring its schema (exactly as
// the fs plugin self-registers tool.fs.read.call's schema). Returns nil when the
// body contributes nothing to the catalog.
type Describer func(s engine.Subscriber) []engine.EventKind

// registry is the process-global kind → factory table; describers is its sibling
// kind → catalog-contribution table. Both are guarded by mu so a daemon can
// register at startup while requests resolve; registration is expected at
// composition time, resolution/description during apply/load.
var (
	mu         sync.RWMutex
	registry   = map[string]Factory{}
	describers = map[string]Describer{}
)

// Register installs a body factory under a kind. Re-registering a kind replaces
// it (the composition root owns the table; last wins). A nil factory is a
// programming error.
func Register(kind string, f Factory) {
	if f == nil {
		panic("nodes: nil factory for kind " + kind)
	}
	mu.Lock()
	defer mu.Unlock()
	registry[kind] = f
}

// RegisterCatalog installs a Describer for a body kind (alongside Register), so a
// body that self-describes a tool contributes its catalog kinds + schema when it
// is wired. The composition root calls it for such bodies (e.g. the changeset
// bridge: topology.apply.requested{document} + its fail kind). A nil describer is
// a programming error.
func RegisterCatalog(kind string, d Describer) {
	if d == nil {
		panic("nodes: nil describer for kind " + kind)
	}
	mu.Lock()
	defer mu.Unlock()
	describers[kind] = d
}

// CatalogFor returns the catalog kinds the subscriber's body self-describes, or
// nil when its body kind has no Describer. The daemon folds these into the
// changeset so the body's tool kinds + schema are registered when it is wired.
func CatalogFor(s engine.Subscriber) []engine.EventKind {
	mu.RLock()
	d, ok := describers[s.BodyKind]
	mu.RUnlock()
	if !ok {
		return nil
	}
	return d(s)
}

// Resolver returns an engine.BodyResolver backed by the registry: it dispatches
// on the body kind to the registered factory. An unknown kind is an error (the
// changeset is rejected / the load fails), naming the kinds that ARE registered
// so the operator sees the gap.
func Resolver() engine.BodyResolver {
	return func(s engine.Subscriber) (engine.Reaction, error) {
		mu.RLock()
		f, ok := registry[s.BodyKind]
		mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("nodes: no factory for body kind %q (registered: %v)", s.BodyKind, registered())
		}
		return f(s)
	}
}

// registered lists the registered kinds, sorted — for the unknown-kind error.
func registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
