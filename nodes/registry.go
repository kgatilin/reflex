// Package nodes is the body-factory registry that sits ABOVE the kernel (doc 20
// / CONCEPT §8, §12): it maps a node's declared body kind ("llm", a tool, …) to
// a factory that builds the runnable engine.Reaction from the node's serialized
// config. The engine stays body-agnostic — it carries the body descriptor
// (kind + config) on the sys.node.registered fact and asks an injected
// engine.BodyResolver to turn it into code. This package IS that resolver: the
// composition root (a daemon, a test) registers the factories it wants
// (nodes.Register("llm", llm.Factory)) and hands engine.WithBodyResolver(
// nodes.Resolver()) to the engine, so a declarative or replayed topology gets
// runnable bodies. Behaviour is code resolved by name; wiring is a fact on the
// log (G8) — the same split as view-type builders.
package nodes

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/kgatilin/reflex/engine"
)

// Factory builds a node's Reaction from its name and opaque config (the body
// descriptor's BodyConfig). It is the per-kind half of an engine.BodyResolver.
type Factory func(name string, config json.RawMessage) (engine.Reaction, error)

// registry is the process-global kind → factory table. It is guarded by a mutex
// so a daemon can register at startup while requests resolve; registration is
// expected at composition time, resolution during apply/load.
var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
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

// Resolver returns an engine.BodyResolver backed by the registry: it dispatches
// on the body kind to the registered factory. An unknown kind is an error (the
// changeset is rejected / the load fails), naming the kinds that ARE registered
// so the operator sees the gap.
func Resolver() engine.BodyResolver {
	return func(name, kind string, config json.RawMessage) (engine.Reaction, error) {
		mu.RLock()
		f, ok := registry[kind]
		mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("nodes: no factory for body kind %q (registered: %v)", kind, registered())
		}
		return f(name, config)
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
