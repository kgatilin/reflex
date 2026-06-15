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
	"strings"
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
	mu           sync.Mutex
	e            *engine.Engine
	plugins      *proxy.Manager
	pluginDecls  []engine.Decl
	launchedCmds map[string]bool // command signature → launched, so a plugins: entry spawns once
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

// New builds a daemon with a fresh engine and the body resolver installed, plus
// the changeset delta expander that backs plugin-handler nodes by the process
// handling their kind. The expander runs inside engine.commitChangeset, so BOTH
// an operator Apply and an in-graph node-emitted changeset get plugin backing —
// a topology a brain node builds at runtime wires its fs/pytest hands exactly as
// a file-applied one does.
func New() *Daemon {
	registerFactories()
	d := &Daemon{plugins: proxy.NewManager(), launchedCmds: map[string]bool{}}
	d.e = engine.New(engine.WithBodyResolver(d.resolver()), engine.WithDeclExpander(d.expandPluginRefs))
	return d
}

// Load builds a daemon by reconstructing the engine from a persisted/replayed
// log (the restart path): every descriptor body is rebuilt from the log via the
// resolver — plugin nodes re-spawn their process on demand (no apply-time probe;
// their schemas are already on the log as sys.event.registered facts).
func Load(log []engine.Event) (*Daemon, error) {
	registerFactories()
	d := &Daemon{plugins: proxy.NewManager(), launchedCmds: map[string]bool{}}
	e, err := engine.Load(log, engine.WithBodyResolver(d.resolver()), engine.WithDeclExpander(d.expandPluginRefs))
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

	if err := d.launchSectionPlugins(doc.Plugins); err != nil {
		return err
	}
	// Plugin-ref expansion now runs inside the engine (WithDeclExpander), so it
	// applies uniformly to this operator Apply and to any in-graph changeset a
	// node emits. The daemon only launches the plugins and folds their not-yet-live
	// catalog kinds ahead of the operator delta.
	pending := d.pendingPluginDecls()
	delta := make([]engine.Decl, 0, len(pending)+len(decls))
	delta = append(delta, pending...)
	delta = append(delta, decls...)
	return d.e.Apply(ctx, delta...)
}

// launchSectionPlugins spawns the document's plugins: entries — the registration
// of out-of-process infrastructure, separate from any subscription. Each entry is
// just HOW to launch (command + transport); the process announces in its hello
// which kinds it handles/emits, and Launch turns that into catalog kinds. A
// command is spawned at most once across Apply/Validate (launchedCmds), so
// re-applying a document does not replace a live process. Only stdio transport is
// supported today; an empty transport defaults to it.
func (d *Daemon) launchSectionPlugins(plugins []topology.PluginSpec) error {
	for _, p := range plugins {
		if len(p.Command) == 0 {
			return fmt.Errorf("daemon: plugin %q: command is required", p.Name)
		}
		if p.Transport != "" && p.Transport != "stdio" {
			return fmt.Errorf("daemon: plugin %q: unsupported transport %q (only stdio)", p.Name, p.Transport)
		}
		sig := strings.Join(p.Command, "\x00")
		if d.launchedCmds[sig] {
			continue
		}
		name, decls, err := d.plugins.Launch(p.Command)
		if err != nil {
			return err
		}
		if p.Name != "" && p.Name != name {
			return fmt.Errorf("daemon: plugin command %v announced name %q, but the document names it %q", p.Command, name, p.Name)
		}
		d.pluginDecls = append(d.pluginDecls, decls...)
		d.launchedCmds[sig] = true
	}
	return nil
}

// expandPluginRefs backs the document's subscribers with their plugin process,
// when one handles their subscribed kind. A handler is a NORMAL subscriber — its
// own On/In/Emits, no plugin reference — and the link to the process is the kind:
// a subscriber whose On (defaulting to [Name]) is handled by a launched plugin is
// backed by it. The resolved body carries the spawn command (so the registered
// fact is rebuildable from the log, G8) and Emits defaults to the plugin's
// produced kinds. A subscriber whose kind no plugin handles is left untouched (an
// ordinary in-graph consumer). The legacy explicit form — body kind "plugin",
// config {plugin: <name>} — is still resolved for callers that name the plugin.
func (d *Daemon) expandPluginRefs(decls []engine.Decl) ([]engine.Decl, error) {
	out := make([]engine.Decl, 0, len(decls))
	for _, dcl := range decls {
		sub, ok := dcl.(engine.Subscriber)
		if !ok {
			out = append(out, dcl)
			continue
		}
		switch sub.BodyKind {
		case proxy.Kind:
			resolved, err := d.resolvePluginRef(sub)
			if err != nil {
				return nil, err
			}
			sub = resolved
		case "":
			resolved, err := d.backByKind(sub)
			if err != nil {
				return nil, err
			}
			sub = resolved
		}
		out = append(out, sub)
	}
	return out, nil
}

// backByKind backs a vanilla subscriber with the launched plugin that HANDLES its
// subscribed kind, if any. The subscription is the link — On (or [Name] when On
// is empty) is matched against each plugin's handled kinds. On a match the body
// becomes the resolved plugin command, On is pinned to the matched kind, and Emits
// defaults to the plugin's produced kinds. No match leaves the subscriber as is.
func (d *Daemon) backByKind(sub engine.Subscriber) (engine.Subscriber, error) {
	kinds := sub.On
	if len(kinds) == 0 {
		kinds = []string{sub.Name}
	}
	for _, k := range kinds {
		name, command, outKinds, ok := d.plugins.HandlerFor(k)
		if !ok {
			continue
		}
		if len(sub.On) == 0 {
			sub.On = []string{k}
		}
		if len(sub.Emits) == 0 {
			sub.Emits = outKinds
		}
		// Carry the plugin NAME (not just the command) so the body keys its client
		// by plugin identity — every handler backed by this plugin shares one
		// process (the plugin's read-before-edit guard et al. stay coherent).
		raw, err := json.Marshal(proxy.Config{Command: command, Plugin: name})
		if err != nil {
			return sub, err
		}
		sub.BodyKind = proxy.Kind
		sub.BodyConfig = raw
		return sub, nil
	}
	return sub, nil
}

// resolvePluginRef resolves the legacy explicit form: body kind "plugin" with a
// {plugin: <name>} reference. It fills the spawn command and defaults On/Emits
// from the named plugin's self-description; the scope (In) stays the operator's.
func (d *Daemon) resolvePluginRef(sub engine.Subscriber) (engine.Subscriber, error) {
	var cfg proxy.Config
	if len(sub.BodyConfig) > 0 {
		if err := json.Unmarshal(sub.BodyConfig, &cfg); err != nil {
			return sub, fmt.Errorf("daemon: plugin subscriber %q body config: %w", sub.Name, err)
		}
	}
	if cfg.Plugin == "" {
		return sub, nil
	}
	command, in, outKinds, found := d.plugins.Resolve(cfg.Plugin)
	if !found {
		return sub, fmt.Errorf("daemon: subscriber %q references plugin %q, which is not launched", sub.Name, cfg.Plugin)
	}
	if len(sub.On) == 0 {
		sub.On = in
	}
	if len(sub.Emits) == 0 {
		sub.Emits = outKinds
	}
	raw, err := json.Marshal(proxy.Config{Command: command, Plugin: cfg.Plugin})
	if err != nil {
		return sub, err
	}
	sub.BodyConfig = raw
	return sub, nil
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
	if err := d.launchSectionPlugins(doc.Plugins); err != nil {
		d.mu.Unlock()
		return engine.Report{}, err
	}
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
