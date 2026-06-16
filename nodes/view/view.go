// Package view is the view-as-reducer body (doc 33 §9d): the developer's primary
// programming surface. A View is a STATEFUL reducer over its scope's events — it
// unifies the View face (its cached state) and the Reaction face (its emits) into
// one object. The engine holds the state per scope instance (a cache rebuilt from
// the log by replay, G8) and threads it through Reduce, so the body stays pure.
//
// The graph never reads this code — it reads the node's declared On/In/Emits, the
// same standardisation the `llm` body uses (CONCEPT §A.4). So a View is a normal
// engine.Subscriber whose body kind is "view": wiring is static (the graph), logic
// is code (the reducer). Reducer types self-register by name (Register); a node
// selects one via BodyConfig {type}.
package view

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
)

// Reducer is the developer interface: fold one event into the view's state and
// optionally emit a transition (CDC, e.g. state.status.waiting). Pure — the engine
// owns the state and passes it in (nil on an instance's first event ⇒ mint the
// zero value).
type Reducer interface {
	Reduce(state any, ev engine.Event) (next any, emits []engine.Emit)
}

// registry maps a reducer type name → its constructor. A reducer self-registers in
// init; a node selects it by BodyConfig.type. Process-global and static, like the
// view-type and provider registries.
var registry = map[string]func() Reducer{}

// Register installs a reducer type. Re-registering replaces (the composition root
// owns the table). A nil constructor is a programming error.
func Register(typ string, mk func() Reducer) {
	if mk == nil {
		panic("view: nil reducer constructor for type " + typ)
	}
	registry[typ] = mk
}

// Config is the view body descriptor: which registered reducer to run.
type Config struct {
	Type string `json:"type"`
}

// body adapts a Reducer to an engine body. It satisfies engine.Reaction (so it is
// a valid Subscriber.Body) AND engine.Reducer (so the engine dispatches it through
// Reduce, threading the per-instance state). React is never called — the engine
// prefers the Reducer path by type assertion.
type body struct{ r Reducer }

func (body) React(context.Context, engine.Event, engine.Views) ([]engine.Emit, error) {
	return nil, nil
}

func (b body) Reduce(state any, ev engine.Event) (any, []engine.Emit) {
	return b.r.Reduce(state, ev)
}

// Factory builds a view body from its config {type}. Register with
// nodes.Register("view", view.Factory).
func Factory(s engine.Subscriber) (engine.Reaction, error) {
	var cfg Config
	if len(s.BodyConfig) > 0 {
		if err := json.Unmarshal(s.BodyConfig, &cfg); err != nil {
			return nil, err
		}
	}
	mk, ok := registry[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("view: unknown reducer type %q (config: %s)", cfg.Type, s.BodyConfig)
	}
	return body{r: mk()}, nil
}
