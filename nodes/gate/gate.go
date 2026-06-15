// Package gate is the terminal-gate body (doc 29 §4b "terminal", doc 30 §4
// "terminal / lifecycle sink"): a node that watches a state-transition kind and
// emits a terminal fact ONLY when a chosen payload field reaches one of a set of
// terminal values. It is the place "the status spine reached a terminal value"
// becomes the single drive-to-terminal fact a `reflexd emit --wait` blocks on.
//
// It is a conditional emit, not the relay anti-pattern (doc 30 §4b): it carries
// the terminal predicate (which field, which values). When the field is not
// terminal it emits nothing — the state spine keeps turning.
//
// Config (BodyConfig):
//
//	emit:  the kind to emit when the gate opens (required, e.g. request.terminal).
//	field: the payload field to read (default "status").
//	when:  the terminal values that open the gate (required, e.g. ["done","failed"]).
//
// The trigger payload is passed through to the emitted fact so the terminal
// reason (the status value) rides along.
package gate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
)

// Config is the gate body's descriptor.
type Config struct {
	Emit  string   `json:"emit"`
	Field string   `json:"field,omitempty"`
	When  []string `json:"when"`
}

// Factory is the nodes.Factory for body kind "gate". Register it with
// nodes.Register("gate", gate.Factory).
func Factory(s engine.Subscriber) (engine.Reaction, error) {
	config := s.BodyConfig
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Emit == "" {
		return nil, fmt.Errorf("gate: no emit kind in config")
	}
	if len(cfg.When) == 0 {
		return nil, fmt.Errorf("gate: no terminal values in config (when)")
	}
	field := cfg.Field
	if field == "" {
		field = "status"
	}
	terminal := map[string]struct{}{}
	for _, v := range cfg.When {
		terminal[v] = struct{}{}
	}

	return engine.ReactionFunc(func(_ context.Context, ev engine.Event, _ engine.Views) ([]engine.Emit, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return nil, nil // an unreadable payload is not terminal; the spine continues
		}
		raw, ok := m[field]
		if !ok {
			return nil, nil
		}
		var val string
		if err := json.Unmarshal(raw, &val); err != nil {
			return nil, nil
		}
		if _, isTerminal := terminal[val]; !isTerminal {
			return nil, nil // not a terminal value — emit nothing, keep turning
		}
		payload := ev.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		return []engine.Emit{{Kind: cfg.Emit, Payload: payload}}, nil
	}), nil
}
