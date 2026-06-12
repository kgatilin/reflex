// Package tool is the acting body (docs/24-concept.md §3): it consumes
// tool.{name}.call and emits tool.{name}.result or tool.{name}.failed.
// An in-process pure function and an out-of-process plugin are one
// concept — where the function runs is a deployment detail, invisible in
// the topology.
package tool

import (
	"context"
	"encoding/json"

	"github.com/kgatilin/reflex/engine"
)

// Func is the whole contract of an in-process tool: input payload in,
// result payload out, error becomes the .failed event. Confinement
// (root clamps, allowlists) lives inside the function — the engine is
// payload-blind (§9).
type Func func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

// Node wires a tool function as a complete topology node: subscribed to
// tool.{name}.call, emitting tool.{name}.result / tool.{name}.failed.
func Node(name string, fn Func) engine.Node {
	return engine.Node{
		Name:  name,
		On:    []string{"tool." + name + ".call"},
		Emits: []string{"tool." + name + ".result", "tool." + name + ".failed"},
		Body:  reaction(name, fn),
	}
}

// reaction adapts Func to the engine contract: result and failure are
// both ordinary events into the cone (G3) — a failed tool still quiesces
// its scope, and the consumer of the closed fold decides what failure
// means.
func reaction(name string, fn Func) engine.Reaction {
	return engine.ReactionFunc(func(ctx context.Context, ev engine.Event, _ engine.Views) ([]engine.Emit, error) {
		out, err := fn(ctx, ev.Payload)
		if err != nil {
			p, _ := json.Marshal(map[string]string{"error": err.Error()})
			return []engine.Emit{{Kind: "tool." + name + ".failed", Payload: p}}, nil
		}
		return []engine.Emit{{Kind: "tool." + name + ".result", Terminal: true, Payload: out}}, nil
	})
}
