package proxy

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/plugin"
)

// Manager owns the plugin processes for one daemon. It spawns each plugin once,
// keyed by node name, and hands the same Client to the apply-time probe (which
// reads the plugin's self-description to wire and catalog it) and to the body
// resolver (which builds the node's Reaction) — so a node is backed by exactly
// one process. State lives on the Manager, not the process-global nodes
// registry, so several daemons can coexist in one process without clobbering
// each other's plugins.
type Manager struct {
	mu      sync.Mutex
	clients map[string]*plugin.Client
}

// NewManager builds an empty manager.
func NewManager() *Manager { return &Manager{clients: map[string]*plugin.Client{}} }

// Ensure returns the client for node name, spawning the process if it is not
// already running. spawned reports whether this call started it, so a caller can
// roll the process back if the surrounding apply is rejected.
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

// Drop closes and forgets the plugin for node name (apply rollback / removal).
func (m *Manager) Drop(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[name]; ok {
		_ = c.Close()
		delete(m.clients, name)
	}
}

// Factory is the resolver entry for the "plugin" kind: it reuses the client the
// apply-time probe spawned, or spawns on demand (the engine.Load path, where
// there is no probe).
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
