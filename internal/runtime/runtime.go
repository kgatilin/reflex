// Package runtime wires a YAML config into a live bus.
//
// It is internal because the wiring is not a public API: callers go through
// cmd/reflex or write their own glue.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/graph"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/projection"
)

// Result is what the runtime returns to the CLI after a single user message.
type Result struct {
	RequestID  string
	Events     []event.Event
	State      projection.SessionState
	Projection *projection.Store
}

// Build constructs a bus from cfg with handlers built via the supplied
// registry. The bus is fully wired but no events have been emitted yet.
//
// Build also compiles the static handler graph and refuses to return a bus
// when the graph contains an uncapped cycle: reflex would rather refuse to
// start than silently loop forever. Declared loops (cycles with a
// max_iterations cap) are honoured by installing per-handler caps on the
// dispatcher.
//
// Phase 4b: the YAML config is no longer just parsed-and-registered in
// place. Each handler is wrapped in a describedSub so the bus emits the
// control-plane events (HandlerRegistered, Subscribed) into the log as it
// is built. The static graph cycle check still runs as a fast pre-flight;
// the bus also runs its live-table cycle check after registration to
// confirm the resulting topology is sound.
func Build(cfg *config.File, reg *handler.Registry) (*bus.Bus, error) {
	g, err := graph.Build(cfg, reg)
	if err != nil {
		return nil, err
	}
	store := event.NewStore()
	proj := projection.NewStore()
	opts := []bus.Option{bus.WithProjection(proj)}
	if cfg.Settings.MaxSteps > 0 {
		opts = append(opts, bus.WithMaxSteps(cfg.Settings.MaxSteps))
	}
	if caps := g.Caps(); len(caps) > 0 {
		opts = append(opts, bus.WithLoopCaps(caps))
	}
	b := bus.New(store, opts...)

	// Phase 4c: seed boot-time grants BEFORE any handlers register. The
	// permission table must be hot by the time control-plane events start
	// flowing so the audit handler sees grants and registrations in the
	// expected order.
	applyBootPermissions(b, cfg)

	for _, h := range cfg.Handlers {
		sub, err := reg.Build(h)
		if err != nil {
			return nil, fmt.Errorf("runtime: build %q: %w", h.Name, err)
		}
		scope := h.Scope
		if scope == "" {
			scope = "default." + h.Name
		}
		// Inline `permissions:` blocks are syntactic sugar for a
		// top-level entry referring to this handler — emit the grant
		// before the handler registers so the table is consistent
		// regardless of which form was used.
		if h.Permissions != nil {
			b.ApplyBootGrant(h.Name, permSpecFromConfig(*h.Permissions))
		}
		// Default implicit grant for scope-less handlers: they own
		// default.<name> and can mutate within default.*. Keeps the
		// Phase 1-4b examples working without YAML changes.
		if h.Scope == "" && h.Permissions == nil {
			b.ApplyBootGrant(h.Name, bus.PermSpec{
				Mutate: []string{"default.*"},
				Read:   []string{"*"},
			})
		}
		// If the handler self-describes (e.g. the audit handler with its
		// custom multi-consume set), prefer its descriptor.
		if d, ok := sub.(bus.Described); ok {
			desc := d.Descriptor()
			if desc.Name == "" {
				desc.Name = h.Name
			}
			if desc.Scope == "" {
				desc.Scope = scope
			}
			b.Register(describedSubFor(sub, desc))
			continue
		}
		spec, ok := reg.ResolveSpec(h)
		if !ok {
			// No spec → register without control-plane descriptor.
			b.Register(sub)
			continue
		}
		desc := descriptorFromSpec(h.Name, spec)
		desc.Scope = scope
		b.Register(describedSubFor(sub, desc))
	}
	b.WireProjection()
	// Defence-in-depth: the live-table cycle check now also has the right
	// to refuse. The YAML pre-check above is the fast path; this catches
	// any drift between the parsed YAML and the actual descriptors that
	// reached the bus.
	if scc, ok := b.CheckLiveTableCycles(); !ok {
		return nil, fmt.Errorf("bus: live-table cycle detected: %v (no cap declared)", scc)
	}
	return b, nil
}

// applyBootPermissions emits a PermissionGranted control-plane event
// per top-level permissions entry. The bus's ApplyBootGrant fold-and-
// emit pair populates the runtime table at the same time the event
// lands on the log — so the boot stream and the table never disagree.
func applyBootPermissions(b *bus.Bus, cfg *config.File) {
	for _, p := range cfg.Permissions {
		b.ApplyBootGrant(p.Principal, permSpecFromConfig(p.Grants))
	}
}

func permSpecFromConfig(g config.GrantsConfig) bus.PermSpec {
	return bus.PermSpec{
		Mutate:    append([]string(nil), g.Mutate...),
		Read:      append([]string(nil), g.Read...),
		Forbidden: append([]string(nil), g.Forbidden...),
		MetaGrant: append([]string(nil), g.MetaGrant...),
	}
}

// descriptorFromSpec converts a handler.HandlerSpec into a
// bus.HandlerDescriptor for the control-plane events.
func descriptorFromSpec(name string, spec handler.HandlerSpec) bus.HandlerDescriptor {
	d := bus.HandlerDescriptor{
		Name:        name,
		Consumes:    spec.Consumes,
		Description: spec.Description,
	}
	for _, e := range spec.Emits {
		d.Emits = append(d.Emits, bus.EmittedDescriptor{
			Type:     e.Type,
			Terminal: e.Terminal,
			Optional: e.Optional,
		})
	}
	return d
}

// describedSub is a thin wrapper around a bus.Subscriber that also exposes
// a HandlerDescriptor via the bus.Described interface. The runtime
// constructs one per YAML handler so the bus can emit HandlerRegistered +
// Subscribed control-plane events when Register is called.
type describedSub struct {
	bus.Subscriber
	desc bus.HandlerDescriptor
}

func (d *describedSub) Descriptor() bus.HandlerDescriptor { return d.desc }

// SetProjection passes through to the wrapped subscriber when it
// implements ProjectionAware. Without this delegation, WireProjection
// would skip wrapped handlers.
func (d *describedSub) SetProjection(p *projection.Store) {
	if pa, ok := d.Subscriber.(bus.ProjectionAware); ok {
		pa.SetProjection(p)
	}
}

func describedSubFor(sub bus.Subscriber, desc bus.HandlerDescriptor) bus.Subscriber {
	return &describedSub{Subscriber: sub, desc: desc}
}

// Run seeds a RequestReceived event with the user's message, drains the
// bus, and runs CheckQuiescence to flag unhandled requests.
func Run(ctx context.Context, b *bus.Bus, message string) (*Result, error) {
	reqID := uuid.NewString()
	payload, err := json.Marshal(map[string]string{"payload": message})
	if err != nil {
		return nil, err
	}
	seed := event.Event{
		Type:      projection.TypeRequestReceived,
		RequestID: reqID,
		Source:    "cli",
		Payload:   payload,
	}
	if err := b.Run(ctx, seed); err != nil {
		return nil, err
	}
	if err := handler.CheckQuiescence(ctx, b); err != nil {
		return nil, err
	}
	all := b.Store().Snapshot()
	state := projection.SessionProjection(all, reqID)
	return &Result{
		RequestID:  reqID,
		Events:     all,
		State:      state,
		Projection: b.Projection(),
	}, nil
}
