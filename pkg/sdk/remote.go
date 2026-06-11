package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

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

// remoteTransport drives one Unix-socket connection to a daemon.
// Phase 4b: a single connection hosts N handlers. The daemon's deliver
// frames carry the handler_name; this transport demultiplexes them to
// the right callback via a name → handler map.
type remoteTransport struct {
	conn net.Conn

	closeMu sync.Mutex
	closed  bool

	// proj RPC plumbing.
	rpcMu      sync.Mutex
	rpcSeq     uint64
	rpcWaiters map[string]chan Frame
}

func (t *remoteTransport) run(ctx context.Context, handlers []*Handler) error {
	if len(handlers) == 0 {
		return errors.New("sdk: remote: no handlers registered")
	}
	t.rpcWaiters = map[string]chan Frame{}

	enc := json.NewEncoder(t.conn)
	encMu := &sync.Mutex{}
	reader := bufio.NewReaderSize(t.conn, 1<<20)

	send := func(f Frame) error {
		encMu.Lock()
		defer encMu.Unlock()
		return enc.Encode(f)
	}

	// Register every handler via its own Hello/Welcome handshake on the
	// same connection.
	for _, h := range handlers {
		if err := send(Frame{Kind: KindHello, Version: ProtocolVersion, Handler: ptr(h.Spec())}); err != nil {
			return fmt.Errorf("sdk: hello %q: %w", h.name, err)
		}
		line, err := readLine(reader)
		if err != nil {
			return fmt.Errorf("sdk: read welcome %q: %w", h.name, err)
		}
		wf, err := DecodeFrame(line)
		if err != nil {
			return fmt.Errorf("sdk: decode welcome: %w", err)
		}
		if wf.Kind == KindError {
			return fmt.Errorf("sdk: daemon error on hello: %s", wf.Error)
		}
		if wf.Kind != KindWelcome {
			return fmt.Errorf("sdk: expected welcome, got %q", wf.Kind)
		}
	}

	// Index handlers by name for delivery routing.
	byName := make(map[string]*Handler, len(handlers))
	for _, h := range handlers {
		byName[h.name] = h
	}

	// Cancellation: close the conn when ctx fires.
	go func() {
		<-ctx.Done()
		_ = t.conn.Close()
	}()

	// Inbound loop.
	for {
		line, err := readLine(reader)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return nil
		}
		f, err := DecodeFrame(line)
		if err != nil {
			return fmt.Errorf("sdk: decode: %w", err)
		}
		switch f.Kind {
		case KindDeliver:
			if f.Event == nil {
				return fmt.Errorf("sdk: deliver missing event")
			}
			h, ok := byName[f.HandlerName]
			if !ok {
				// Daemon sent us a deliver for a handler we don't host —
				// nack so the daemon doesn't hang waiting.
				_ = send(Frame{Kind: KindNack, DeliveryID: f.DeliveryID, Error: fmt.Sprintf("unknown handler %q", f.HandlerName)})
				continue
			}
			// Dispatch in a goroutine so the inbound loop keeps reading
			// (the handler may need round-trip RPCs like proj_get which
			// arrive on this same connection while it runs).
			ev := *f.Event
			deliveryID := f.DeliveryID
			go t.dispatch(ctx, send, h, deliveryID, ev)
		case KindProjValue:
			t.deliverRPC(f)
		case KindError:
			return fmt.Errorf("sdk: daemon: %s", f.Error)
		case KindGoodbye:
			return nil
		default:
			// Unknown but non-fatal.
		}
	}
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

func (t *remoteTransport) dispatch(ctx context.Context, send func(Frame) error, h *Handler, deliveryID string, ev event.Event) {
	rc := &remoteCtx{
		ctx:        ctx,
		parent:     ev,
		deliveryID: deliveryID,
		send:       send,
		transport:  t,
		emits:      emitsByType(h),
	}
	err := h.fn(rc, ev)
	if err != nil {
		_ = send(Frame{Kind: KindNack, DeliveryID: deliveryID, Error: err.Error()})
		return
	}
	_ = send(Frame{Kind: KindAck, DeliveryID: deliveryID})
}

// deliverRPC matches an incoming proj_value frame to its waiting RPC
// goroutine.
func (t *remoteTransport) deliverRPC(f Frame) {
	t.rpcMu.Lock()
	ch, ok := t.rpcWaiters[f.RPCID]
	if ok {
		delete(t.rpcWaiters, f.RPCID)
	}
	t.rpcMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- f:
	default:
	}
}

// nextRPCID returns a fresh RPC correlation ID.
func (t *remoteTransport) nextRPCID() string {
	t.rpcMu.Lock()
	defer t.rpcMu.Unlock()
	t.rpcSeq++
	return fmt.Sprintf("rpc-%d", t.rpcSeq)
}

// registerRPC creates a waiter channel for the given RPC ID and returns
// it. The inbound loop's deliverRPC drains entries.
func (t *remoteTransport) registerRPC(id string) chan Frame {
	ch := make(chan Frame, 1)
	t.rpcMu.Lock()
	t.rpcWaiters[id] = ch
	t.rpcMu.Unlock()
	return ch
}

// remoteCtx is the Ctx implementation for the remote transport. Emits go
// over the wire tagged with delivery_id so the daemon-side React collects
// them and returns them to the bus.
type remoteCtx struct {
	ctx        context.Context
	parent     event.Event
	deliveryID string
	send       func(Frame) error
	transport  *remoteTransport
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
	return c.send(Frame{Kind: KindEmit, DeliveryID: c.deliveryID, Event: &ev})
}

// ProjectionGet round-trips a proj_get frame to the daemon. Blocks until
// the daemon replies with proj_value (or the context fires). On error or
// timeout, returns (nil, false) so callers degrade gracefully.
func (c *remoteCtx) ProjectionGet(key string) (any, bool) {
	if c.transport == nil {
		return nil, false
	}
	id := c.transport.nextRPCID()
	ch := c.transport.registerRPC(id)
	frame := Frame{
		Kind:      KindProjGet,
		RPCID:     id,
		Key:       key,
		RequestID: c.parent.RequestID,
	}
	if err := c.send(frame); err != nil {
		return nil, false
	}
	select {
	case f := <-ch:
		if !f.Found {
			return nil, false
		}
		var v any
		if err := json.Unmarshal(f.Value, &v); err != nil {
			return nil, false
		}
		return v, true
	case <-c.ctx.Done():
		return nil, false
	case <-time.After(10 * time.Second):
		return nil, false
	}
}

// ProjectionSet sends a fire-and-forget proj_set frame to the daemon.
// Errors during marshal / send are silently swallowed (callers can't
// react to them inside a handler anyway).
func (c *remoteCtx) ProjectionSet(key string, value any) {
	if c.transport == nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	_ = c.send(Frame{
		Kind:      KindProjSet,
		Key:       key,
		Value:     raw,
		RequestID: c.parent.RequestID,
	})
}

func ptr[T any](v T) *T { return &v }
