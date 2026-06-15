package proxy

import (
	"fmt"
	"sync"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/plugin"
)

// Manager owns the plugin processes for one daemon. It spawns each plugin once,
// keyed by the plugin's announced name, and hands the same Client to Launch
// (which reads the plugin's self-description to derive its subscriber node +
// catalog kinds) and to the body resolver (which builds that node's Reaction) —
// so a node is backed by exactly one process. State lives on the Manager, not the
// process-global nodes registry, so several daemons can coexist in one process
// without clobbering each other's plugins.
type Manager struct {
	mu       sync.Mutex
	clients  map[string]*plugin.Client
	launched map[string]launchInfo
}

// launchInfo is what a launched plugin contributes to operator wiring: its spawn
// command (so a subscriber referencing it by name is rebuildable from the log,
// G8) and its self-described kinds (so an operator subscriber can default its
// On/Emits from the plugin's capability).
type launchInfo struct {
	command []string
	spec    plugin.Spec
}

// NewManager builds an empty manager.
func NewManager() *Manager {
	return &Manager{clients: map[string]*plugin.Client{}, launched: map[string]launchInfo{}}
}

// Launch spawns a plugin process, adopts its client, and returns the decls its
// self-description (hello) contributes to the topology — WITHOUT applying them.
// A plugin's job is to expose a CAPABILITY: "I handle these kinds (with schemas),
// I emit these kinds (with schemas)." Launching contributes exactly that — one
// EventKind per declared kind+schema, in AND out — and nothing else.
//
// It does NOT create a subscriber. Whether this agent USES the plugin, and in
// which scope, is the operator's decision, declared in the topology like any
// other subscription: a Subscriber with body kind "plugin" referencing the
// launched plugin by name (config {plugin: <name>}), with an operator-chosen In.
// The daemon resolves that reference back to this command and the plugin's kinds
// (Resolve). Launching exposes capability; the topology decides the wiring.
func (m *Manager) Launch(command []string) (name string, decls []engine.Decl, err error) {
	c, err := plugin.Spawn(command...)
	if err != nil {
		return "", nil, err
	}
	spec := c.Spec()
	name = spec.Name
	if name == "" {
		_ = c.Close()
		return "", nil, fmt.Errorf("proxy: plugin %v announced no name in its hello", command)
	}
	m.adopt(name, c)
	m.mu.Lock()
	m.launched[name] = launchInfo{command: append([]string(nil), command...), spec: spec}
	m.mu.Unlock()

	for _, ev := range spec.Events {
		decls = append(decls, engine.EventKind{Kind: ev.Kind, Schema: ev.Schema})
	}
	return name, decls, nil
}

// Resolve returns a launched plugin's spawn command and its self-described kinds
// (In = handled, Out = produced) so the daemon can expand an operator subscriber
// that references the plugin by name. ok is false if no plugin of that name has
// been launched.
func (m *Manager) Resolve(name string) (command []string, in, out []string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.launched[name]
	if !ok {
		return nil, nil, nil, false
	}
	return append([]string(nil), l.command...), l.spec.In(), l.spec.Out(), true
}

// adopt stores an already-spawned client under name (the launch path), closing
// and replacing any prior client for that name (a relaunch).
func (m *Manager) adopt(name string, c *plugin.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.clients[name]; ok {
		_ = old.Close()
	}
	m.clients[name] = c
}

// Ensure returns the client for node name, spawning the process if it is not
// already running (the engine.Load path, where the resolver rebuilds a plugin
// body from its descriptor and no launch has pre-adopted a client). spawned
// reports whether this call started it.
func (m *Manager) Ensure(name string, command []string) (client *plugin.Client, spawned bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[name]; ok {
		return c, false, nil
	}
	c, err := plugin.Spawn(command...)
	if err != nil {
		return nil, false, err
	}
	m.clients[name] = c
	return c, true, nil
}

// Factory is the resolver entry for the "plugin" kind: it reuses the client the
// launch path adopted, or spawns on demand (the engine.Load path, where the
// resolver rebuilds the body from its descriptor and no launch pre-adopted one).
func (m *Manager) Factory(s engine.Subscriber) (engine.Reaction, error) {
	cfg, err := ParseConfig(s.Name, s.BodyConfig)
	if err != nil {
		return nil, err
	}
	c, _, err := m.Ensure(s.Name, cfg.Command)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", s.Name, err)
	}
	return reaction{name: s.Name, client: c}, nil
}

// Close tears down every plugin (daemon shutdown).
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, c := range m.clients {
		_ = c.Close()
		delete(m.clients, name)
	}
	return nil
}
