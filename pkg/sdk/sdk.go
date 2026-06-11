package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/kgatilin/reflex/pkg/event"
)

// Event is the SDK's view of an inbound event. It is a thin alias over
// event.Event so SDK users don't have to import the internal event package
// for ordinary inspection; they still can if they want the full type.
type Event = event.Event

// Args is a convenience map for emitting events without hand-marshalling
// JSON. ctx.Emit("Type", sdk.Args{"k":"v"}) is equivalent to
// ctx.EmitRaw("Type", json.Marshal({"k":"v"})).
type Args map[string]any

// HandlerFunc is the user-supplied reaction. It receives a Ctx (for emitting
// and consulting the projection store) and the inbound event.
type HandlerFunc func(ctx Ctx, ev Event) error

// Handler is the SDK-side description of one reactive subscription. Build
// via NewHandler + Option helpers, then attach a HandlerFunc with OnEvent,
// and register it on a Client.
type Handler struct {
	name     string
	consumes string
	emits    []EmittedSpec
	fn       HandlerFunc
}

// NewHandler constructs a Handler. The Option arguments declare what the
// handler consumes / emits — they mirror the YAML `on:` / `emits:` /
// terminal fields so an SDK-defined handler is observationally
// indistinguishable from a YAML-declared one.
func NewHandler(name string, opts ...Option) *Handler {
	h := &Handler{name: name}
	for _, o := range opts {
		o(h)
	}
	return h
}

// OnEvent installs the reaction. Returns the handler for chaining.
func (h *Handler) OnEvent(fn HandlerFunc) *Handler {
	h.fn = fn
	return h
}

// Name returns the handler's name.
func (h *Handler) Name() string { return h.name }

// Spec returns the wire-level handler spec so the client can announce it
// to the daemon (or the in-process registrar).
func (h *Handler) Spec() HandlerSpec {
	emits := make([]EmittedSpec, len(h.emits))
	copy(emits, h.emits)
	return HandlerSpec{
		Name:     h.name,
		Consumes: h.consumes,
		Emits:    emits,
	}
}

// Option mutates a Handler at construction.
type Option func(*Handler)

// Consumes declares the inbound event type the handler reacts to. Required.
func Consumes(eventType string) Option {
	return func(h *Handler) { h.consumes = eventType }
}

// Emits declares one outbound event type. Multiple Emits options accumulate.
func Emits(eventType string) Option {
	return func(h *Handler) {
		h.emits = append(h.emits, EmittedSpec{Type: eventType})
	}
}

// Terminal marks the given outbound event type (already declared with Emits)
// as terminal. If the type is not yet declared it is added.
func Terminal(eventType string) Option {
	return func(h *Handler) {
		for i := range h.emits {
			if h.emits[i].Type == eventType {
				h.emits[i].Terminal = true
				return
			}
		}
		h.emits = append(h.emits, EmittedSpec{Type: eventType, Terminal: true})
	}
}

// Optional marks the given outbound event type as not guaranteed.
func Optional(eventType string) Option {
	return func(h *Handler) {
		for i := range h.emits {
			if h.emits[i].Type == eventType {
				h.emits[i].Optional = true
				return
			}
		}
		h.emits = append(h.emits, EmittedSpec{Type: eventType, Optional: true})
	}
}

// Ctx is the handler-side context passed into a HandlerFunc. It is the SDK's
// uniform way to emit follow-up events and read the projection store.
type Ctx interface {
	// Context returns the underlying Go context (for cancellation).
	Context() context.Context

	// RequestID returns the request_id of the event being handled.
	RequestID() string

	// Emit queues a follow-up event. Returns when the event has been
	// accepted by the bus (in-process) or sent over the wire (remote).
	// Returns ErrTerminalEvent if the inbound event was terminal.
	Emit(eventType string, args Args) error

	// EmitRaw is the JSON-bytes form of Emit.
	EmitRaw(eventType string, payload json.RawMessage) error

	// EmitEvent is the full-control form: caller supplies a partial
	// event.Event (ID/TS/Source/CausedBy/RequestID are auto-filled).
	EmitEvent(ev event.Event) error

	// ProjectionGet reads key from the projection store for this request.
	// Remote transport: this is a round-trip RPC to the daemon and may be
	// added later; Phase 4a in-process always works, remote panics with
	// "not yet implemented" if called.
	ProjectionGet(key string) (any, bool)

	// ProjectionSet writes key=value into the projection store. Remote:
	// out of scope for 4a — handlers that need projection writes should
	// emit an event the daemon-side aggregator picks up.
	ProjectionSet(key string, value any)
}

// ErrTerminalEvent is returned by Ctx.Emit when the incoming event was
// terminal — terminal events are leaves of the causal DAG and must not
// spawn descendants.
var ErrTerminalEvent = errors.New("sdk: incoming event was terminal; emit denied")

// ErrClosed is returned by Client methods after the client has shut down.
var ErrClosed = errors.New("sdk: client closed")

// Connect dials the transport described by opt and returns a connected
// Client ready to register handlers and Run.
func Connect(opt TransportOption) (*Client, error) {
	if opt == nil {
		return nil, errors.New("sdk: nil transport option")
	}
	t, err := opt.build()
	if err != nil {
		return nil, err
	}
	return &Client{transport: t}, nil
}

// TransportOption is the result of one of the transport constructors
// (InProcess, Remote). It is an interface so the user-facing call site
// stays one line: sdk.Connect(sdk.Remote("…")).
type TransportOption interface {
	build() (transport, error)
}

// Client is the user-facing handle.
type Client struct {
	transport transport

	mu       sync.Mutex
	handlers []*Handler
	running  bool
	closed   bool
}

// Register attaches h to the client. Must be called before Run. The same
// client can host multiple handlers (each gets its own subscription).
func (c *Client) Register(h *Handler) error {
	if h == nil {
		return errors.New("sdk: nil handler")
	}
	if h.fn == nil {
		return fmt.Errorf("sdk: handler %q has no OnEvent callback", h.name)
	}
	if h.consumes == "" {
		return fmt.Errorf("sdk: handler %q has no Consumes declaration", h.name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.running {
		return errors.New("sdk: Register must be called before Run")
	}
	c.handlers = append(c.handlers, h)
	return nil
}

// Run starts dispatch. For InProcess it returns nil immediately after
// installing the handlers on the underlying bus (the bus drains synchronously
// when something is emitted; there is nothing for Run to block on). For
// Remote it blocks until ctx is cancelled or the daemon disconnects.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.running {
		c.mu.Unlock()
		return errors.New("sdk: Run already called")
	}
	c.running = true
	handlers := append([]*Handler(nil), c.handlers...)
	c.mu.Unlock()
	return c.transport.run(ctx, handlers)
}

// Close shuts the client down. Safe to call multiple times.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.transport.close()
}

// transport is the internal interface every concrete transport implements.
type transport interface {
	run(ctx context.Context, handlers []*Handler) error
	close() error
}
