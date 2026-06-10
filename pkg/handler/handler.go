// Package handler holds the built-in reflex handler types and the registry
// that maps YAML-declared `type:` strings to live Subscribers.
//
// A handler is a factory: given a HandlerConfig it returns a bus.Subscriber.
// The factory is the only place that interprets the per-handler `config:`
// block, so adding a new handler type means writing one factory and one
// registry line.
//
// Phase 1.5: handlers are also self-describing. Every registered type
// supplies a HandlerSpec that lists the event type it consumes and the event
// types it may emit (with per-emission Terminal/Optional flags). The
// Introspect API exposes this as a projection over the registry so the
// static graph builder, the validate/describe subcommands, and (in later
// phases) the daemon and analyzer can reason about the handler topology
// without instantiating a single handler.
package handler

import (
	"context"
	"fmt"
	"sort"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// HandlerSpec is the self-description of one handler type. It is loaded into
// the registry alongside the factory and queried via the Introspect API.
//
// Consumes is "*" when the event subscribed to is decided by the YAML's
// `on:` field rather than baked into the handler implementation. The graph
// builder resolves the runtime consumed type from the YAML in that case.
type HandlerSpec struct {
	Type        string        // YAML `type:` discriminator
	Description string        // human-readable, single sentence
	Consumes    string        // event type subscribed to; "*" if dynamic-from-config
	Emits       []EmittedSpec // possible emitted events
}

// EmittedSpec describes one of a handler's possible emissions.
//
// Terminal mirrors event.Event.Terminal: the emission closes its causal
// branch and never spawns descendants. Optional=true means the emission only
// happens under some inputs; Optional=false means it is guaranteed for any
// reaction.
type EmittedSpec struct {
	Type     string
	Terminal bool
	Optional bool
}

// SelfDescribing is implemented by handlers (or their factories) that want
// to publish their spec programmatically. The registry stores the spec
// directly; this interface exists so adapters can re-export it.
type SelfDescribing interface {
	Spec() HandlerSpec
}

// Factory builds a Subscriber from a YAML handler entry.
type Factory func(cfg config.HandlerConfig) (bus.Subscriber, error)

// SpecResolver derives a per-instance HandlerSpec from a YAML entry. For
// handlers whose emissions are statically known the resolver just returns
// the type-level spec; for echo it substitutes the configured emit type.
type SpecResolver func(cfg config.HandlerConfig, base HandlerSpec) HandlerSpec

// Registry maps handler type names to their factories AND their specs. The
// pairing is intentional: keeping the factory and the spec next to each
// other makes it impossible to ship a handler the introspection API can't
// see.
type Registry struct {
	factories map[string]Factory
	specs     map[string]HandlerSpec
	resolvers map[string]SpecResolver
}

// NewRegistry returns an empty registry. Use BuiltinRegistry for the default
// reflex handler set.
func NewRegistry() *Registry {
	return &Registry{
		factories: map[string]Factory{},
		specs:     map[string]HandlerSpec{},
		resolvers: map[string]SpecResolver{},
	}
}

// Register adds a factory + spec under spec.Type. Re-registering the same
// name returns an error. An optional SpecResolver can be supplied to derive
// per-instance specs from the YAML entry (e.g. echo's dynamic Emit type).
func (r *Registry) Register(spec HandlerSpec, f Factory, resolver ...SpecResolver) error {
	if spec.Type == "" {
		return fmt.Errorf("handler: spec.Type is required")
	}
	if _, ok := r.factories[spec.Type]; ok {
		return fmt.Errorf("handler: type %q already registered", spec.Type)
	}
	r.factories[spec.Type] = f
	r.specs[spec.Type] = spec
	if len(resolver) > 0 && resolver[0] != nil {
		r.resolvers[spec.Type] = resolver[0]
	}
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

// ListTypes returns the sorted list of registered handler type names.
//
// Part of the Introspect contract.
func (r *Registry) ListTypes() []string {
	out := make([]string, 0, len(r.specs))
	for k := range r.specs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SpecOf returns the HandlerSpec registered under typeName.
//
// Part of the Introspect contract.
func (r *Registry) SpecOf(typeName string) (HandlerSpec, bool) {
	s, ok := r.specs[typeName]
	return s, ok
}

// ResolveSpec returns the per-instance HandlerSpec for cfg. It applies the
// registered SpecResolver if any (so echo's dynamic emit shows up); it falls
// back to the type-level spec when no resolver is registered. The Consumes
// field is always set to cfg.On when the type-level spec is "*".
func (r *Registry) ResolveSpec(cfg config.HandlerConfig) (HandlerSpec, bool) {
	base, ok := r.specs[cfg.Type]
	if !ok {
		return HandlerSpec{}, false
	}
	spec := base
	if resolver, ok := r.resolvers[cfg.Type]; ok && resolver != nil {
		spec = resolver(cfg, base)
	}
	if spec.Consumes == "*" || spec.Consumes == "" {
		spec.Consumes = cfg.On
	}
	return spec, true
}

// Emitters returns the sorted list of handler types whose spec includes
// eventType in its Emits set.
//
// Part of the Introspect contract.
func (r *Registry) Emitters(eventType string) []string {
	var out []string
	for typ, spec := range r.specs {
		for _, em := range spec.Emits {
			if em.Type == eventType {
				out = append(out, typ)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Consumers returns the sorted list of handler types whose spec consumes
// eventType. Handlers with Consumes=="*" (dynamic-from-config) are always
// included, since they could subscribe to any type.
//
// Part of the Introspect contract.
func (r *Registry) Consumers(eventType string) []string {
	var out []string
	for typ, spec := range r.specs {
		if spec.Consumes == eventType || spec.Consumes == "*" {
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	return out
}

// Introspect is the read-only projection over the registry that downstream
// packages (graph builder, validate CLI, describe CLI, future daemon) depend
// on. *Registry implements it.
type Introspect interface {
	ListTypes() []string
	SpecOf(typeName string) (HandlerSpec, bool)
	Emitters(eventType string) []string
	Consumers(eventType string) []string
}

// Compile-time interface check.
var _ Introspect = (*Registry)(nil)

// BuiltinRegistry returns a Registry populated with reflex's stock handler
// types: llm_stub, tool_call, printer, terminator, unhandled_watcher, echo,
// plus the triage-pipeline trio (parse_target, gh_query, triage_rules).
// These cover the calc, stall, triage, and loop examples.
func BuiltinRegistry() *Registry {
	r := NewRegistry()
	must(r.Register(llmStubSpec(), newLLMStub, llmStubSpecResolver))
	must(r.Register(toolCallSpec(), newToolCall))
	must(r.Register(printerSpec(), newPrinter))
	must(r.Register(terminatorSpec(), newTerminator))
	must(r.Register(unhandledWatcherSpec(), newUnhandledWatcher))
	must(r.Register(echoSpec(), newEcho, echoSpecResolver))
	must(r.Register(parseTargetSpec(), newParseTarget))
	must(r.Register(ghQuerySpec(), newGhQuery))
	must(r.Register(triageRulesSpec(), newTriageRules))
	must(r.Register(aggregatorSpec(), newAggregator, aggregatorSpecResolver))
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
