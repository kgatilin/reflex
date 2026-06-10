// Package handler holds the built-in reflex handler types and the registry
// that maps YAML-declared `type:` strings to live Subscribers.
//
// A handler is a factory: given a HandlerConfig it returns a bus.Subscriber.
// The factory is the only place that interprets the per-handler `config:`
// block, so adding a new handler type means writing one factory and one
// registry line.
package handler

import (
	"context"
	"fmt"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// Factory builds a Subscriber from a YAML handler entry.
type Factory func(cfg config.HandlerConfig) (bus.Subscriber, error)

// Registry maps handler type names to their factories.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry. Use BuiltinRegistry for the default
// reflex handler set.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Register adds a factory under typeName. Re-registering the same name
// returns an error.
func (r *Registry) Register(typeName string, f Factory) error {
	if _, ok := r.factories[typeName]; ok {
		return fmt.Errorf("handler: type %q already registered", typeName)
	}
	r.factories[typeName] = f
	return nil
}

// Types returns the set of known type names for config validation.
func (r *Registry) Types() map[string]bool {
	out := map[string]bool{}
	for k := range r.factories {
		out[k] = true
	}
	return out
}

// Build invokes the factory for cfg.Type.
func (r *Registry) Build(cfg config.HandlerConfig) (bus.Subscriber, error) {
	f, ok := r.factories[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("handler: no factory for %q", cfg.Type)
	}
	return f(cfg)
}

// BuiltinRegistry returns a Registry populated with reflex's stock handler
// types: llm_stub, tool_call, printer, terminator, unhandled_watcher, and
// echo. These cover the calc and stall examples.
func BuiltinRegistry() *Registry {
	r := NewRegistry()
	must(r.Register("llm_stub", newLLMStub))
	must(r.Register("tool_call", newToolCall))
	must(r.Register("printer", newPrinter))
	must(r.Register("terminator", newTerminator))
	must(r.Register("unhandled_watcher", newUnhandledWatcher))
	must(r.Register("echo", newEcho))
	return r
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// baseSub stores the name common to every handler.
type baseSub struct{ name string }

func (b baseSub) Name() string { return b.name }

// runFn is the simple form of a React implementation.
type runFn func(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error)

// genericSub is a small adapter for handlers whose behavior fits a closure.
type genericSub struct {
	baseSub
	on  string
	run runFn
}

func (g *genericSub) Match(ev event.Event) bool { return ev.Type == g.on }
func (g *genericSub) React(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
	return g.run(ctx, ev, log)
}
