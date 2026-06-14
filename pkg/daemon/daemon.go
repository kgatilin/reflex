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
	"encoding/json"
	"sync"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes"
	"github.com/kgatilin/reflex/nodes/llm"
	"github.com/kgatilin/reflex/nodes/proxy"
	"github.com/kgatilin/reflex/pkg/topology"
)

// Daemon hosts one engine and serializes access to it. It owns a per-daemon
// plugin Manager so plugin processes are scoped to this daemon, not the
// process-global nodes registry.
type Daemon struct {
	mu      sync.Mutex
	e       *engine.Engine
	plugins *proxy.Manager
}

// registerFactories wires the in-process body kinds into the process registry.
// Idempotent (Register replaces), so constructing several daemons in one process
// (tests) is safe. "llm" is the only true in-process body (the reasoning core);
// every hand (fs, pytest, …) is an out-of-process plugin handled per-daemon by
// the plugin Manager via resolver() — not registered here (doc 29 Iteration 3).
func registerFactories() {
	nodes.Register("llm", llm.Factory)
}

// New builds a daemon with a fresh engine and the body resolver installed.
func New() *Daemon {
	registerFactories()
	d := &Daemon{plugins: proxy.NewManager()}
	d.e = engine.New(engine.WithBodyResolver(d.resolver()))
	return d
}

// Load builds a daemon by reconstructing the engine from a persisted/replayed
// log (the restart path): every descriptor body is rebuilt from the log via the
// resolver — plugin nodes re-spawn their process on demand (no apply-time probe;
// their schemas are already on the log as sys.event.registered facts).
func Load(log []engine.Event) (*Daemon, error) {
	registerFactories()
	d := &Daemon{plugins: proxy.NewManager()}
	e, err := engine.Load(log, engine.WithBodyResolver(d.resolver()))
	if err != nil {
		return nil, err
	}
	d.e = e
	return d, nil
}

// resolver composes the global nodes registry (llm, …) with this daemon's own
// plugin Manager for the "plugin" kind, so plugin processes are owned per-daemon.
func (d *Daemon) resolver() engine.BodyResolver {
	base := nodes.Resolver()
	return func(name, kind string, config json.RawMessage) (engine.Reaction, error) {
		if kind == proxy.Kind {
			return d.plugins.Factory(name, config)
		}
		return base(name, kind, config)
	}
}

// Close tears down the daemon's plugin processes (shutdown).
func (d *Daemon) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.plugins.Close()
}

// Apply decodes a document into decls, probes any plugin nodes for their
// self-description, expands the changeset accordingly, then runs it
// (engine.Apply): the resulting graph is validated as a whole and applied, or
// rejected. The error is the engine's (a *engine.ValidationError carries the gap
// Report). On rejection, any plugin spawned by this call is rolled back.
func (d *Daemon) Apply(ctx context.Context, doc topology.Document) error {
	decls, err := doc.Decls()
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	expanded, spawned, err := d.expandPlugins(decls)
	if err != nil {
		d.rollback(spawned)
		return err
	}
	if err := d.e.Apply(ctx, expanded...); err != nil {
		d.rollback(spawned)
		return err
	}
	return nil
}

// expandPlugins probes every plugin node, spawning its process (once) to read
// its announced self-description, and rewrites the changeset: the node's On/Emits
// gain the plugin's consumed/emitted kinds, and one EventKind decl is added per
// declared kind+schema — so the catalog is populated dynamically from the plugin,
// never hardcoded. spawned names the processes this call started (for rollback).
func (d *Daemon) expandPlugins(decls []engine.Decl) (out []engine.Decl, spawned []string, err error) {
	seenKind := map[string]bool{}
	for _, dcl := range decls {
		if ek, ok := dcl.(engine.EventKind); ok {
			seenKind[ek.Kind] = true
		}
	}
	for _, dcl := range decls {
		n, ok := dcl.(engine.Node)
		if !ok || n.BodyKind != proxy.Kind {
			out = append(out, dcl)
			continue
		}
		cfg, perr := proxy.ParseConfig(n.Name, n.BodyConfig)
		if perr != nil {
			return nil, spawned, perr
		}
		client, didSpawn, serr := d.plugins.Ensure(n.Name, cfg.Command)
		if serr != nil {
			return nil, spawned, serr
		}
		if didSpawn {
			spawned = append(spawned, n.Name)
		}
		spec := client.Spec()
		n.On = union(n.On, spec.In())
		n.Emits = union(n.Emits, spec.Out())
		out = append(out, n)
		for _, ev := range spec.Events {
			if seenKind[ev.Kind] {
				continue
			}
			seenKind[ev.Kind] = true
			out = append(out, engine.EventKind{Kind: ev.Kind, Schema: ev.Schema})
		}
	}
	return out, spawned, nil
}

func (d *Daemon) rollback(spawned []string) {
	for _, name := range spawned {
		d.plugins.Drop(name)
	}
}

// union appends to a the elements of b not already present, preserving order.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			a = append(a, s)
		}
	}
	return a
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
