// Package daemon is the long-lived host around the engine (doc 29 Iteration 2 /
// CONCEPT §8): a single engine.Engine with the body-factory resolver wired in,
// guarded by a mutex so the API can apply topology, emit events, and read views
// concurrently against an engine that is itself single-threaded. It is the
// composition root — it registers the body factories ("llm" → llm.Factory) and
// installs nodes.Resolver() into the engine. The HTTP server (server.go) is a
// thin transport over these methods; an in-process caller (tests, an embedded
// run) uses them directly.
package daemon

import (
	"context"
	"sync"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes"
	"github.com/kgatilin/reflex/nodes/llm"
	"github.com/kgatilin/reflex/nodes/proxy"
	"github.com/kgatilin/reflex/pkg/topology"
)

// Daemon hosts one engine and serializes access to it.
type Daemon struct {
	mu sync.Mutex
	e  *engine.Engine
}

// registerFactories wires the built-in body kinds into the process registry.
// Idempotent (Register replaces), so constructing several daemons in one process
// (tests) is safe. "llm" is the only true in-process body (the reasoning core);
// every hand (fs, pytest, …) is an out-of-process plugin behind the "plugin"
// proxy kind — the daemon spawns it over stdio (doc 29 Iteration 3).
func registerFactories() {
	nodes.Register("llm", llm.Factory)
	nodes.Register(proxy.Kind, proxy.Factory)
}

// New builds a daemon with a fresh engine and the body resolver installed.
func New() *Daemon {
	registerFactories()
	return &Daemon{e: engine.New(engine.WithBodyResolver(nodes.Resolver()))}
}

// Load builds a daemon by reconstructing the engine from a persisted/replayed
// log (the restart path): every descriptor body is rebuilt from the log via the
// resolver (engine.Load).
func Load(log []engine.Event) (*Daemon, error) {
	registerFactories()
	e, err := engine.Load(log, engine.WithBodyResolver(nodes.Resolver()))
	if err != nil {
		return nil, err
	}
	return &Daemon{e: e}, nil
}

// Apply decodes a document into decls and runs the changeset (engine.Apply): the
// resulting graph is validated as a whole and applied, or rejected. The error is
// the engine's (a *engine.ValidationError carries the gap Report).
func (d *Daemon) Apply(ctx context.Context, doc topology.Document) error {
	decls, err := doc.Decls()
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.e.Apply(ctx, decls...)
}

// Validate is the read-only dry-run of a document against the CURRENT live table
// (the same resulting-graph validator Apply uses), appending nothing. It folds
// the live decls plus the document's decls and reports the gaps.
func (d *Daemon) Validate(doc topology.Document) (engine.Report, error) {
	decls, err := doc.Decls()
	if err != nil {
		return engine.Report{}, err
	}
	d.mu.Lock()
	live := d.e.Topology()
	d.mu.Unlock()
	resulting := make([]engine.Decl, 0, len(live)+len(decls))
	resulting = append(resulting, live...)
	resulting = append(resulting, decls...)
	return engine.Validate(resulting...)
}

// Emit appends one ingress event and, when drain is set, drives the engine to
// quiescence. It returns the events produced from this ingress onward (the slice
// appended at or after the ingress), so a caller can inspect the reconciliation
// — including any terminal fact — without re-reading the whole log.
func (d *Daemon) Emit(ctx context.Context, subject string, payload []byte, drain bool) ([]engine.Event, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	before := d.logLen()
	if _, err := d.e.Append(ctx, subject, payload); err != nil {
		return nil, err
	}
	if drain {
		if err := d.e.Drain(ctx); err != nil {
			return nil, err
		}
	}
	return d.eventsFrom(before), nil
}

// Topology returns the live table (the fold of the changeset facts).
func (d *Daemon) Topology() []engine.Decl {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.e.Topology()
}

// Events returns the whole log in order (a snapshot copy).
func (d *Daemon) Events() []engine.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot()
}

// logLen / snapshot / eventsFrom read the log under the held lock.
func (d *Daemon) logLen() int { return len(d.snapshot()) }

func (d *Daemon) snapshot() []engine.Event {
	var out []engine.Event
	for ev := range d.e.Events() {
		out = append(out, ev)
	}
	return out
}

func (d *Daemon) eventsFrom(from int) []engine.Event {
	all := d.snapshot()
	if from < 0 || from > len(all) {
		return nil
	}
	return all[from:]
}
