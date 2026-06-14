package proxy

import (
	"encoding/json"
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
	mu      sync.Mutex
	clients map[string]*plugin.Client
}

// NewManager builds an empty manager.
func NewManager() *Manager { return &Manager{clients: map[string]*plugin.Client{}} }

// Launch spawns a plugin process, adopts its client, and returns the decls its
// self-description (hello) contributes to the topology — WITHOUT applying them.
// A plugin's whole job is to expose "I handle these kinds (with schemas), I emit
// these kinds (with schemas)" and to subscribe to handle them; Launch turns that
// announcement into:
//
//   - one GLOBAL subscriber Node (On = the kinds it handles, Emits = the kinds it
//     produces, body = a plugin descriptor carrying the spawn command so the body
//     is rebuildable from the log, G8); and
//   - one EventKind per declared kind+schema, in AND out — the plugin announces
//     schemas for both what it consumes and what it produces.
//
// The subscriber's scope is "global" on purpose: a plugin is scope-agnostic — it
// handles its kinds wherever they occur, and the engine places its emits in the
// trigger's cone by causality (a reaction never chooses its emit's scope). WHO
// emits the handled kinds and WHO consumes the results is the separate topology
// wiring (the operator's graph), not the plugin's concern. The caller folds these
// decls into its next changeset alongside the operator document, where the graph
// is validated as a whole (a launched-but-unwired plugin is correctly a gap).
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

	cfg, err := json.Marshal(Config{Command: command})
	if err != nil {
		return "", nil, err
	}
	decls = append(decls, engine.Node{
		Name: name, On: spec.In(), Emits: spec.Out(), In: "global",
		BodyKind: Kind, BodyConfig: cfg,
	})
	for _, ev := range spec.Events {
		decls = append(decls, engine.EventKind{Kind: ev.Kind, Schema: ev.Schema})
	}
	return name, decls, nil
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
func (m *Manager) Factory(name string, config json.RawMessage) (engine.Reaction, error) {
	cfg, err := ParseConfig(name, config)
	if err != nil {
		return nil, err
	}
	c, _, err := m.Ensure(name, cfg.Command)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", name, err)
	}
	return reaction{name: name, client: c}, nil
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
