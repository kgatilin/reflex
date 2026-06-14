// Package proxy is the engine adapter for the out-of-process plugin seam (doc 29
// Iteration 3): it registers the "plugin" body kind. A node declaring
// body_kind: plugin carries a spawn command in its body_config; the Factory
// spawns that child process (stdio transport) and returns an engine.Reaction
// that, on each firing, forwards the triggering event to the plugin and returns
// the emits it produces. The engine binds those emits to the node's declared
// emit set as usual — out-of-process is not out-of-bounds.
//
// The plugin process is spawned at Apply time (Factory call): applying a
// topology that references a missing/broken plugin binary fails the changeset,
// and engine.Load re-spawns the plugins on restart from the body descriptors on
// the log (G8 — the wiring is recoverable; the live process is not, exactly like
// a live Body closure).
package proxy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/plugin"
)

// Kind is the registered body kind for a plugin-backed node.
const Kind = "plugin"

// Config is a plugin node's body_config: the command the daemon execs to launch
// the plugin process, e.g. ["reflexd", "plugin", "fs"] or ["python", "pytest_plugin.py"].
type Config struct {
	Command []string `json:"command"`
}

// Factory is the engine.BodyResolver entry for the "plugin" kind. It is wired
// into the registry by the composition root: nodes.Register(proxy.Kind, proxy.Factory).
func Factory(name string, config json.RawMessage) (engine.Reaction, error) {
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("proxy %q: decoding body_config: %w", name, err)
		}
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("proxy %q: body_config.command is required", name)
	}
	client, err := plugin.Spawn(cfg.Command...)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", name, err)
	}
	return reaction{name: name, client: client}, nil
}

// reaction adapts one plugin Client to the engine. Views are not forwarded —
// hands react to the event, not to in-process projections.
type reaction struct {
	name   string
	client *plugin.Client
}

func (r reaction) React(ctx context.Context, ev engine.Event, _ engine.Views) ([]engine.Emit, error) {
	emits, err := r.client.Invoke(ctx, plugin.Event{Subject: ev.Subject, Payload: ev.Payload})
	if err != nil {
		return nil, err
	}
	out := make([]engine.Emit, len(emits))
	for i, e := range emits {
		out[i] = engine.Emit{Kind: e.Kind, Payload: e.Payload}
	}
	return out, nil
}
