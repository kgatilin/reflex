// Package proxy is the engine adapter for the out-of-process plugin seam (doc 29
// Iteration 3): it backs the "plugin" body kind. The subscriber node for a plugin
// is NOT operator-declared — the daemon launches a plugin, reads its self-
// description, and generates a node whose body_config carries the spawn command
// (proxy.Manager.Launch). That body descriptor (kind "plugin" + config) rides on
// sys.node.registered, so the Factory can rebuild the engine.Reaction from it: on
// each firing the reaction forwards the triggering event to the plugin process
// (stdio transport) and returns the emits it produces. The engine binds those
// emits to the node's declared emit set as usual — out-of-process is not
// out-of-bounds.
//
// engine.Load re-spawns the plugins on restart from those descriptors (G8 — the
// wiring is recoverable; the live process is not, exactly like a live Body
// closure). A descriptor referencing a missing/broken plugin binary fails the
// load (or the changeset that introduces it).
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

// ParseConfig decodes and validates a plugin node's body_config.
func ParseConfig(name string, config json.RawMessage) (Config, error) {
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return Config{}, fmt.Errorf("proxy %q: decoding body_config: %w", name, err)
		}
	}
	if len(cfg.Command) == 0 {
		return Config{}, fmt.Errorf("proxy %q: body_config.command is required", name)
	}
	return cfg, nil
}

// Factory is a standalone engine.BodyResolver entry for the "plugin" kind: it
// spawns a fresh process per call. The daemon uses a Manager instead (spawn-once
// at launch + reuse when the resolver rebuilds the body); Factory is for direct
// and test use where no manager is needed.
func Factory(name string, config json.RawMessage) (engine.Reaction, error) {
	cfg, err := ParseConfig(name, config)
	if err != nil {
		return nil, err
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
	emits, err := r.client.Invoke(ctx, plugin.Event{Kind: engine.KindOf(ev), Subject: ev.Subject, Payload: ev.Payload})
	if err != nil {
		return nil, err
	}
	out := make([]engine.Emit, len(emits))
	for i, e := range emits {
		out[i] = engine.Emit{Kind: e.Kind, Payload: e.Payload}
	}
	return out, nil
}
