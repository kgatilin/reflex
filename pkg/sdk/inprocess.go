package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// InProcess wraps an existing *bus.Bus so SDK handlers run in the same
// process as the bus. No socket, no serialization — the SDK handler is
// turned into a bus.Subscriber and registered directly.
func InProcess(b *bus.Bus) TransportOption {
	return &inProcessOption{bus: b}
}

type inProcessOption struct{ bus *bus.Bus }

func (o *inProcessOption) build() (transport, error) {
	if o.bus == nil {
		return nil, errors.New("sdk: InProcess: nil bus")
	}
	return &inProcessTransport{bus: o.bus}, nil
}

type inProcessTransport struct {
	bus *bus.Bus
}

func (t *inProcessTransport) run(ctx context.Context, handlers []*Handler) error {
	for _, h := range handlers {
		t.bus.Register(newInProcessSub(h, t.bus))
	}
	// Wire the projection into anything that wants it (including our subs).
	t.bus.WireProjection()
	// InProcess Run returns immediately. The bus drains synchronously on
	// Emit; there is no event-loop here to block on. Callers who want to
	// keep the process alive should rely on the host (e.g. `reflex run`).
	// We still honour ctx so tests can detect immediate cancellation.
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (t *inProcessTransport) close() error { return nil }

// inProcessSub is the bus.Subscriber adapter for an SDK handler.
type inProcessSub struct {
	h    *Handler
	bus  *bus.Bus
	proj *projection.Store
}

func newInProcessSub(h *Handler, b *bus.Bus) *inProcessSub {
	return &inProcessSub{h: h, bus: b, proj: b.Projection()}
}

func (s *inProcessSub) Name() string { return s.h.name }
func (s *inProcessSub) Match(ev event.Event) bool {
	return ev.Type == s.h.consumes
}

// SetProjection satisfies bus.ProjectionAware so WireProjection (re-)injects
// the store when the bus assembles late.
func (s *inProcessSub) SetProjection(p *projection.Store) { s.proj = p }

// React invokes the handler callback inside a Ctx that buffers emits and
// returns them to the dispatcher (matching YAML-handler semantics: the
// dispatcher assigns IDs/timestamps and routes the result).
func (s *inProcessSub) React(ctx context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
	c := &inProcessCtx{
		ctx:    ctx,
		parent: ev,
		proj:   s.proj,
		emits:  emitsByType(s.h),
	}
	if err := s.h.fn(c, ev); err != nil {
		return nil, err
	}
	return c.emitted, nil
}

// emitsByType indexes the handler's declared emits by event type so the
// dispatcher can paint Terminal onto outbound events without the handler
// callback having to remember the flag at every Emit site.
func emitsByType(h *Handler) map[string]EmittedSpec {
	m := make(map[string]EmittedSpec, len(h.emits))
	for _, e := range h.emits {
		m[e.Type] = e
	}
	return m
}

// inProcessCtx is the Ctx implementation for the in-process transport. All
// emits are buffered; the bus dispatcher picks them up and routes them.
type inProcessCtx struct {
	ctx     context.Context
	parent  event.Event
	proj    *projection.Store
	emits   map[string]EmittedSpec
	emitted []event.Event
}

func (c *inProcessCtx) Context() context.Context { return c.ctx }
func (c *inProcessCtx) RequestID() string         { return c.parent.RequestID }

func (c *inProcessCtx) Emit(eventType string, args Args) error {
	var payload json.RawMessage
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("sdk: Emit %q: %w", eventType, err)
		}
		payload = raw
	}
	return c.EmitRaw(eventType, payload)
}

func (c *inProcessCtx) EmitRaw(eventType string, payload json.RawMessage) error {
	return c.EmitEvent(event.Event{Type: eventType, Payload: payload})
}

func (c *inProcessCtx) EmitEvent(ev event.Event) error {
	if c.parent.Terminal {
		return ErrTerminalEvent
	}
	// Paint Terminal from the handler's declared spec if the caller did
	// not set it explicitly. Mirrors the YAML contract where Terminal is
	// a property of the EMISSION type (declared once), not something each
	// emit site re-asserts.
	if !ev.Terminal {
		if spec, ok := c.emits[ev.Type]; ok && spec.Terminal {
			ev.Terminal = true
		}
	}
	c.emitted = append(c.emitted, ev)
	return nil
}

func (c *inProcessCtx) ProjectionGet(key string) (any, bool) {
	if c.proj == nil {
		return nil, false
	}
	return c.proj.Get(c.parent.RequestID, key)
}

func (c *inProcessCtx) ProjectionSet(key string, value any) {
	if c.proj == nil {
		return
	}
	c.proj.Set(c.parent.RequestID, key, value)
}
