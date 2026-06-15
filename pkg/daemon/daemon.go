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
	"fmt"
	"sync"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes"
	"github.com/kgatilin/reflex/nodes/entry"
	"github.com/kgatilin/reflex/nodes/gate"
	"github.com/kgatilin/reflex/nodes/llm"
	"github.com/kgatilin/reflex/nodes/proxy"
	"github.com/kgatilin/reflex/nodes/verifier"
	"github.com/kgatilin/reflex/pkg/topology"
)

// Daemon hosts one engine and serializes access to it. It owns a per-daemon
// plugin Manager so plugin processes are scoped to this daemon, not the
// process-global nodes registry.
//
// pluginDecls accumulates the CATALOG contributions of the plugins this daemon
// has launched: each connected plugin announces "I handle these kinds (schemas),
// I emit these kinds (schemas)" and Launch turns that into one EventKind per kind
// (proxy.Manager.Launch) — capability, not wiring. The handler SUBSCRIPTION is
// the operator's, declared in the document as a body-kind "plugin" subscriber
// referencing the plugin by name (expandPluginRefs), with an operator-chosen
// scope. These catalog kinds are folded into the next Apply/Validate so the
// plugins' schemas are on the log (the llm body advertises them). Only the kinds
// not yet on the log are passed as the changeset delta (pendingPluginDecls).
type Daemon struct {
	mu          sync.Mutex
	e           *engine.Engine
	plugins     *proxy.Manager
	pluginDecls []engine.Decl
}

// registerFactories wires the in-process body kinds into the process registry.
// Idempotent (Register replaces), so constructing several daemons in one process
// (tests) is safe. "llm" is the reasoning core; entry/gate/verifier are the
// generic control bodies the agent topology wires (doc 29 §4b): entry maps a
// boundary kind to a domain kind, gate drives the terminal fact off a state
// field, verifier turns an agreed check into a verified status. Every hand (fs,
// pytest, …) is an out-of-process plugin handled per-daemon by the plugin Manager
// via resolver() — not registered here (doc 29 Iteration 3).
func registerFactories() {
	nodes.Register("llm", llm.Factory)
	nodes.Register("entry", entry.Factory)
	nodes.Register("gate", gate.Factory)
	nodes.Register("verifier", verifier.Factory)
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
	return func(s engine.Subscriber) (engine.Reaction, error) {
		if s.BodyKind == proxy.Kind {
			return d.plugins.Factory(s)
		}
		return base(s)
	}
}

// Close tears down the daemon's plugin processes (shutdown).
func (d *Daemon) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.plugins.Close()
}

// LaunchPlugin spawns a plugin process and records the CATALOG it announces (the
// kinds it can handle + emit, with schemas) WITHOUT applying anything and WITHOUT
// wiring it into the graph. Launching exposes a capability; whether this agent
// uses the plugin — and in which scope — is the operator's decision, declared in
// the topology as a body-kind "plugin" subscriber referencing the plugin by name
// (config {plugin: <name>}). It returns the plugin's announced name.
//
// This is the seam for an agent extending itself at runtime: launch a new plugin,
// then apply a topology that subscribes a plugin-backed node to its kinds — the
// wiring (the operator's subscription) stays separate from the plugin's
// self-description (its capability).
func (d *Daemon) LaunchPlugin(_ context.Context, command []string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name, decls, err := d.plugins.Launch(command)
	if err != nil {
		return "", err
	}
	d.pluginDecls = append(d.pluginDecls, decls...)
	return name, nil
}

// Apply folds this daemon's pending plugin contributions (subscriber nodes +
// catalog kinds the launched plugins announced, not yet on the log) ahead of the
// operator document and runs the changeset (engine.Apply): the resulting graph —
// live table + plugins + operator decls — is validated as a whole and applied, or
// rejected. The error is the engine's (a *engine.ValidationError carries the gap
// Report). Only the not-yet-live plugin decls are passed as the delta; the engine
// prepends the live table itself, so re-sending a live node would duplicate it.
func (d *Daemon) Apply(ctx context.Context, doc topology.Document) error {
	decls, err := doc.Decls()
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	decls, err = d.expandPluginRefs(decls)
	if err != nil {
		return err
	}
	pending := d.pendingPluginDecls()
	delta := make([]engine.Decl, 0, len(pending)+len(decls))
	delta = append(delta, pending...)
	delta = append(delta, decls...)
	return d.e.Apply(ctx, delta...)
}

// expandPluginRefs resolves operator subscribers that reference a launched plugin
// by name — body kind "plugin", config {plugin: <name>} — into the resolved form:
// it fills the spawn command from the launched plugin (so the registered fact is
// rebuildable from the log, G8) and defaults On/Emits from the plugin's
// self-described kinds when the operator omitted them. The scope (In) stays the
// operator's choice — launching exposes capability, the topology owns the wiring.
// A reference to a plugin that has not been launched is a clean error.
func (d *Daemon) expandPluginRefs(decls []engine.Decl) ([]engine.Decl, error) {
	out := make([]engine.Decl, 0, len(decls))
	for _, dcl := range decls {
		sub, ok := dcl.(engine.Subscriber)
		if !ok || sub.BodyKind != proxy.Kind {
			out = append(out, dcl)
			continue
		}
		var cfg proxy.Config
		if len(sub.BodyConfig) > 0 {
			if err := json.Unmarshal(sub.BodyConfig, &cfg); err != nil {
				return nil, fmt.Errorf("daemon: plugin subscriber %q body config: %w", sub.Name, err)
			}
		}
		if cfg.Plugin != "" {
			command, in, outKinds, found := d.plugins.Resolve(cfg.Plugin)
			if !found {
				return nil, fmt.Errorf("daemon: subscriber %q references plugin %q, which is not launched", sub.Name, cfg.Plugin)
			}
			if len(sub.On) == 0 {
				sub.On = in
			}
			if len(sub.Emits) == 0 {
				sub.Emits = outKinds
			}
			raw, err := json.Marshal(proxy.Config{Command: command})
			if err != nil {
				return nil, err
			}
			sub.BodyConfig = raw
		}
		out = append(out, sub)
	}
	return out, nil
}

// pendingPluginDecls returns the launched plugins' decls that are NOT yet on the
// log (the live table). The engine's changeset takes a delta over the live table
// — a subscriber already registered must not be re-sent (foldSubscribers does not
// dedup, so a duplicate would break the graph build). After a Load the plugin
// subscribers are already live (rebuilt from the log) and pluginDecls is empty,
// so this is nil.
func (d *Daemon) pendingPluginDecls() []engine.Decl {
	if len(d.pluginDecls) == 0 {
		return nil
	}
	liveSubs := map[string]bool{}
	liveEvents := map[string]bool{}
	for _, dcl := range d.e.Topology() {
		switch v := dcl.(type) {
		case engine.Subscriber:
			liveSubs[v.Name] = true
		case engine.EventKind:
			liveEvents[v.Kind] = true
		}
	}
	var out []engine.Decl
	for _, dcl := range d.pluginDecls {
		switch v := dcl.(type) {
		case engine.Subscriber:
			if !liveSubs[v.Name] {
				out = append(out, v)
			}
		case engine.EventKind:
			if !liveEvents[v.Kind] {
				out = append(out, v)
			}
		}
	}
	return out
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
	pending := d.pendingPluginDecls()
	decls, err = d.expandPluginRefs(decls)
	d.mu.Unlock()
	if err != nil {
		return engine.Report{}, err
	}
	resulting := make([]engine.Decl, 0, len(live)+len(pending)+len(decls))
	resulting = append(resulting, live...)
	resulting = append(resulting, pending...)
	resulting = append(resulting, decls...)
	return engine.Validate(resulting...)
}

// Emit appends one external event and, when drain is set, drives the engine to
// quiescence. It returns the events produced from this event onward (the slice
// appended at or after the external event), so a caller can inspect the reconciliation
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
