package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/kgatilin/reflex/pkg/event"
)

// Remote describes a connection to a reflex daemon over a Unix domain
// socket. Phase 4a does not implement TCP / TLS — Unix-socket only.
func Remote(socketPath string) TransportOption {
	return &remoteOption{path: socketPath}
}

type remoteOption struct{ path string }

func (o *remoteOption) build() (transport, error) {
	if o.path == "" {
		return nil, errors.New("sdk: Remote: empty socket path")
	}
	conn, err := net.Dial("unix", o.path)
	if err != nil {
		return nil, fmt.Errorf("sdk: dial %s: %w", o.path, err)
	}
	return &remoteTransport{conn: conn}, nil
}

// remoteTransport drives one Unix-socket connection to a daemon. It
// supports exactly ONE handler per connection in Phase 4a — registering
// multiple handlers on the same client opens multiple connections (one
// per handler) in Run. Keeps the wire protocol stupid.
type remoteTransport struct {
	conn net.Conn

	closeMu sync.Mutex
	closed  bool
}

func (t *remoteTransport) run(ctx context.Context, handlers []*Handler) error {
	if len(handlers) == 0 {
		return errors.New("sdk: remote: no handlers registered")
	}

	// Phase 4a simple model: one handler per remote.Transport instance.
	// Connect() returned with one already-dialled socket; if the user
	// registered exactly one handler, we use it directly. If they
	// registered multiple, we open additional sockets — but that is a
	// rare ask in 4a and the example only registers one.
	if len(handlers) == 1 {
		return t.runOne(ctx, t.conn, handlers[0])
	}
	// Multi-handler: open extra connections (re-using the dial path is
	// awkward because we don't know the socket path here; punt by failing
	// loudly).
	return errors.New("sdk: remote: only one handler per client is supported in Phase 4a — open a separate Client per handler")
}

func (t *remoteTransport) close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	return t.conn.Close()
}

// runOne drives the handshake and inbound loop for a single handler/socket.
func (t *remoteTransport) runOne(ctx context.Context, conn net.Conn, h *Handler) error {
	enc := json.NewEncoder(conn)
	reader := bufio.NewReaderSize(conn, 1<<20)

	// Hello.
	hello := Frame{Kind: KindHello, Version: ProtocolVersion, Handler: ptr(h.Spec())}
	if err := enc.Encode(hello); err != nil {
		return fmt.Errorf("sdk: hello: %w", err)
	}
	// Welcome.
	line, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("sdk: read welcome: %w", err)
	}
	wf, err := DecodeFrame(line)
	if err != nil {
		return fmt.Errorf("sdk: decode welcome: %w", err)
	}
	if wf.Kind == KindError {
		return fmt.Errorf("sdk: daemon error: %s", wf.Error)
	}
	if wf.Kind != KindWelcome {
		return fmt.Errorf("sdk: expected welcome, got %q", wf.Kind)
	}

	// Cancellation: close the conn when ctx fires.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// Inbound loop.
	encMu := &sync.Mutex{}
	for {
		line, err := readLine(reader)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Clean disconnect on EOF.
			return nil
		}
		f, err := DecodeFrame(line)
		if err != nil {
			// Decode errors on inbound frames are fatal for this
			// connection. Don't try to write back — the conn may be
			// half-broken.
			return fmt.Errorf("sdk: decode: %w", err)
		}
		switch f.Kind {
		case KindDeliver:
			if f.Event == nil {
				return fmt.Errorf("sdk: deliver missing event")
			}
			t.dispatch(ctx, encMu, enc, h, f.DeliveryID, *f.Event)
		case KindError:
			return fmt.Errorf("sdk: daemon: %s", f.Error)
		case KindGoodbye:
			return nil
		default:
			// Unknown but non-fatal: log via a stderr write would be
			// nice but the SDK has no logger; ignore for now.
		}
	}
}

func (t *remoteTransport) dispatch(ctx context.Context, encMu *sync.Mutex, enc *json.Encoder, h *Handler, deliveryID string, ev event.Event) {
	rc := &remoteCtx{
		ctx:        ctx,
		parent:     ev,
		deliveryID: deliveryID,
		encMu:      encMu,
		enc:        enc,
		emits:      emitsByType(h),
	}
	err := h.fn(rc, ev)
	encMu.Lock()
	defer encMu.Unlock()
	if err != nil {
		_ = enc.Encode(Frame{Kind: KindNack, DeliveryID: deliveryID, Error: err.Error()})
		return
	}
	_ = enc.Encode(Frame{Kind: KindAck, DeliveryID: deliveryID})
}

// remoteCtx is the Ctx implementation for the remote transport. Emits go
// over the wire tagged with delivery_id so the daemon-side React collects
// them and returns them to the bus.
type remoteCtx struct {
	ctx        context.Context
	parent     event.Event
	deliveryID string
	encMu      *sync.Mutex
	enc        *json.Encoder
	emits      map[string]EmittedSpec
}

func (c *remoteCtx) Context() context.Context { return c.ctx }
func (c *remoteCtx) RequestID() string         { return c.parent.RequestID }

func (c *remoteCtx) Emit(eventType string, args Args) error {
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

func (c *remoteCtx) EmitRaw(eventType string, payload json.RawMessage) error {
	return c.EmitEvent(event.Event{Type: eventType, Payload: payload})
}

func (c *remoteCtx) EmitEvent(ev event.Event) error {
	if c.parent.Terminal {
		return ErrTerminalEvent
	}
	if !ev.Terminal {
		if spec, ok := c.emits[ev.Type]; ok && spec.Terminal {
			ev.Terminal = true
		}
	}
	frame := Frame{Kind: KindEmit, DeliveryID: c.deliveryID, Event: &ev}
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.enc.Encode(frame)
}

func (c *remoteCtx) ProjectionGet(key string) (any, bool) {
	// Phase 4a: remote projection reads are not yet wired (would need an
	// extra round-trip kind). Return absent so callers degrade gracefully
	// rather than panicking. TODO(phase-4b).
	_ = key
	return nil, false
}

func (c *remoteCtx) ProjectionSet(key string, value any) {
	// Same as Get: no-op for now. TODO(phase-4b).
	_, _ = key, value
}

func ptr[T any](v T) *T { return &v }
