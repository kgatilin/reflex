// Package entry is the entry/resolver body (doc 29 §4b, doc 30 §4 "entry"
// role): a node that, when it fires, emits one fixed domain kind. It is the
// legitimate boundary mapper — an external kind (task.new) into a domain root
// kind (request.received), or a system fact (scope.X.budget_exhausted) into a
// state transition. It is NOT the relay anti-pattern (doc 30 §4b): it carries
// data across the boundary (the trigger payload, by default) or stamps a fixed
// payload, and it is the named place a topology turns an outside event into the
// kind that roots a scope.
//
// Config (BodyConfig):
//
//	emit:    the kind to emit (required).
//	payload: an optional fixed JSON payload. When set, every firing emits this
//	         constant (e.g. {"status":"failed"} on budget exhaustion). When
//	         absent, the trigger event's payload is passed through unchanged so
//	         the task text flows task.new → request.received → the brain's view.
//
// The emitted kind must be in the subscriber's Emits allowlist (the engine lints
// emit ⊆ Emits); the body only produces the value, the topology authorises it.
package entry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
)

// Config is the entry body's descriptor: the kind to emit and an optional fixed
// payload (absent ⇒ pass the trigger payload through).
type Config struct {
	Emit    string          `json:"emit"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Factory is the nodes.Factory for body kind "entry": decode the config and
// build the emit Reaction. Register it with nodes.Register("entry", entry.Factory).
func Factory(_ string, config json.RawMessage) (engine.Reaction, error) {
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Emit == "" {
		return nil, fmt.Errorf("entry: no emit kind in config")
	}
	return engine.ReactionFunc(func(_ context.Context, ev engine.Event, _ engine.Views) ([]engine.Emit, error) {
		payload := cfg.Payload
		if len(payload) == 0 {
			payload = ev.Payload // pass the trigger payload through (carry the data across)
		}
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		return []engine.Emit{{Kind: cfg.Emit, Payload: payload}}, nil
	}), nil
}
