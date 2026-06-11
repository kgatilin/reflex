package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// Daemon wraps a *bus.Bus and a Unix-socket listener so external handler
// clients can connect and register subscriptions.
//
// The daemon owns the bus's lifecycle on a "per-request" basis: emit events
// are accepted on the socket, the bus drains, the resulting trace fans out
// to whichever connected remote handlers are subscribed.
//
// Phase 4a scope: subscription mutation goes over the wire as a direct
// bus.Register call. Treating subscriptions as events on the bus is 4b.
type Daemon struct {
	bus      *bus.Bus
	listener net.Listener

	mu      sync.Mutex
	conns   map[*remoteSub]struct{}
	closed  bool
	emitMu  sync.Mutex // serialises bus.Run across remote emit() requests
	nextDID atomic.Uint64
}

// NewDaemon constructs a daemon over the supplied bus. Call Serve(ctx) to
// run the accept loop. Close to release the listener and disconnect clients.
func NewDaemon(b *bus.Bus, listener net.Listener) *Daemon {
	return &Daemon{
		bus:      b,
		listener: listener,
		conns:    map[*remoteSub]struct{}{},
	}
}

// Serve runs the accept loop. Returns when ctx is cancelled or Close is
// called. Errors from individual connections are not propagated — they are
// logged-ish (returned via the per-connection goroutine which terminates
// the client cleanly).
func (d *Daemon) Serve(ctx context.Context) error {
	// Cancel propagation: when ctx is cancelled, close the listener so
	// Accept() unblocks.
	go func() {
		<-ctx.Done()
		_ = d.Close()
	}()

	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.isClosed() {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			// Transient — but for a Unix socket this usually means the
			// listener is gone; treat as fatal.
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

// Close shuts the listener and disconnects clients.
func (d *Daemon) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	conns := make([]*remoteSub, 0, len(d.conns))
	for c := range d.conns {
		conns = append(conns, c)
	}
	d.mu.Unlock()

	for _, c := range conns {
		c.shutdown()
	}
	if d.listener != nil {
		return d.listener.Close()
	}
	return nil
}

// Bus exposes the underlying bus so the daemon command can seed events from
// CLI emit requests, run quiescence checks, etc.
func (d *Daemon) Bus() *bus.Bus { return d.bus }

func (d *Daemon) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// EmitAndDrain takes a seed event from a CLI client (over the socket) and
// drains the bus. Serialised so multiple concurrent CLI clients don't
// trample on each other's drain.
func (d *Daemon) EmitAndDrain(ctx context.Context, seed event.Event) error {
	d.emitMu.Lock()
	defer d.emitMu.Unlock()
	return d.bus.Run(ctx, seed)
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReaderSize(conn, 1<<20) // 1 MiB line cap
	enc := json.NewEncoder(conn)

	// Step 1: handshake.
	line, err := readLine(reader)
	if err != nil {
		_ = enc.Encode(Frame{Kind: KindError, Error: fmt.Sprintf("read hello: %v", err)})
		return
	}
	frame, err := DecodeFrame(line)
	if err != nil {
		_ = enc.Encode(Frame{Kind: KindError, Error: fmt.Sprintf("decode hello: %v", err)})
		return
	}
	if frame.Kind != KindHello || frame.Handler == nil {
		_ = enc.Encode(Frame{Kind: KindError, Error: "expected hello"})
		return
	}
	if frame.Version != 0 && frame.Version != ProtocolVersion {
		_ = enc.Encode(Frame{Kind: KindError, Error: fmt.Sprintf("protocol version %d, expected %d", frame.Version, ProtocolVersion)})
		return
	}
	if frame.Handler.Name == "" || frame.Handler.Consumes == "" {
		_ = enc.Encode(Frame{Kind: KindError, Error: "handler.name and handler.consumes are required"})
		return
	}

	sub := newRemoteSub(d, conn, enc, *frame.Handler)
	d.bus.Register(sub)

	d.mu.Lock()
	d.conns[sub] = struct{}{}
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.conns, sub)
		d.mu.Unlock()
		sub.shutdown()
	}()

	// Welcome.
	if err := enc.Encode(Frame{Kind: KindWelcome, HandlerName: sub.spec.Name, Version: ProtocolVersion}); err != nil {
		return
	}

	// Step 2: per-connection inbound loop. Reads ack/nack/emit/goodbye from
	// the client.
	for {
		line, err := readLine(reader)
		if err != nil {
			return
		}
		f, err := DecodeFrame(line)
		if err != nil {
			_ = enc.Encode(Frame{Kind: KindError, Error: fmt.Sprintf("decode: %v", err)})
			return
		}
		switch f.Kind {
		case KindAck:
			sub.complete(f.DeliveryID, nil)
		case KindNack:
			sub.complete(f.DeliveryID, errors.New(f.Error))
		case KindEmit:
			if f.Event == nil {
				_ = enc.Encode(Frame{Kind: KindError, Error: "emit missing event"})
				continue
			}
			if f.DeliveryID != "" {
				// Emit tied to an in-flight delivery: buffer for React.
				if !sub.addEmit(f.DeliveryID, *f.Event) {
					_ = enc.Encode(Frame{Kind: KindError, Error: fmt.Sprintf("emit: unknown delivery_id %q", f.DeliveryID)})
				}
				continue
			}
			// Bare emit: treat as a fresh seed event. Lets clients drive
			// the bus from outside any delivery cycle (e.g. a `reflex
			// emit … --daemon` CLI shim connects, emits, disconnects).
			ev := *f.Event
			if ev.RequestID == "" {
				ev.RequestID = uuid.NewString()
			}
			if err := d.EmitAndDrain(ctx, ev); err != nil {
				_ = enc.Encode(Frame{Kind: KindError, Error: err.Error()})
			}
		case KindGoodbye:
			return
		default:
			_ = enc.Encode(Frame{Kind: KindError, Error: fmt.Sprintf("unknown kind %q", f.Kind)})
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			// Last line without trailing newline — accept it.
			return line, nil
		}
		return nil, err
	}
	// Strip trailing newline (and optional CR).
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line, nil
}

// remoteSub is the daemon-side bus.Subscriber that proxies events to a
// connected remote handler.
type remoteSub struct {
	daemon *Daemon
	spec   HandlerSpec
	conn   net.Conn
	encMu  sync.Mutex
	enc    *json.Encoder

	mu        sync.Mutex
	pending   map[string]*pendingDelivery
	shutdown1 sync.Once
	dead      chan struct{}
}

// pendingDelivery is the daemon-side state for an in-flight Deliver call.
// React buffers any emit frames the client returns under this delivery_id
// here, then drains them into the returned slice on ack.
type pendingDelivery struct {
	done    chan error
	emitted []event.Event
}

func newRemoteSub(d *Daemon, conn net.Conn, enc *json.Encoder, spec HandlerSpec) *remoteSub {
	return &remoteSub{
		daemon:  d,
		spec:    spec,
		conn:    conn,
		enc:     enc,
		pending: map[string]*pendingDelivery{},
		dead:    make(chan struct{}),
	}
}

func (s *remoteSub) Name() string { return s.spec.Name }
func (s *remoteSub) Match(ev event.Event) bool {
	return ev.Type == s.spec.Consumes
}

// React sends Deliver to the remote handler, blocks waiting for Ack/Nack,
// then returns success/error. Emit frames received with the same delivery_id
// before the ack are collected and returned to the dispatcher as if the
// handler had run in-process — preserving the YAML-handler semantics.
func (s *remoteSub) React(ctx context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
	deliveryID := fmt.Sprintf("d-%d", s.daemon.nextDID.Add(1))
	pd := &pendingDelivery{done: make(chan error, 1)}
	s.mu.Lock()
	s.pending[deliveryID] = pd
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, deliveryID)
		s.mu.Unlock()
	}()

	frame := Frame{Kind: KindDeliver, DeliveryID: deliveryID, Event: &ev}
	s.encMu.Lock()
	err := s.enc.Encode(frame)
	s.encMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("remote-sub %q: send deliver: %w", s.spec.Name, err)
	}

	// Wait for the client's ack/nack, with a generous default deadline so a
	// crashed client doesn't hang the dispatcher forever.
	select {
	case herr := <-pd.done:
		if herr != nil {
			return nil, herr
		}
		// Snapshot emitted under the lock to avoid racing with a late emit.
		s.mu.Lock()
		out := append([]event.Event(nil), pd.emitted...)
		s.mu.Unlock()
		return out, nil
	case <-s.dead:
		return nil, fmt.Errorf("remote-sub %q: connection closed mid-delivery", s.spec.Name)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("remote-sub %q: ack timeout", s.spec.Name)
	}
}

// addEmit attaches a client-emitted event to the in-flight delivery.
// Returns true if the delivery is known, false otherwise (so the connection
// loop can warn / log).
func (s *remoteSub) addEmit(deliveryID string, ev event.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pd, ok := s.pending[deliveryID]
	if !ok {
		return false
	}
	pd.emitted = append(pd.emitted, ev)
	return true
}

func (s *remoteSub) complete(deliveryID string, herr error) {
	s.mu.Lock()
	pd, ok := s.pending[deliveryID]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case pd.done <- herr:
	default:
	}
}

func (s *remoteSub) shutdown() {
	s.shutdown1.Do(func() {
		close(s.dead)
		_ = s.conn.Close()
	})
}

// Ensure remoteSub satisfies bus.ProjectionAware (cheap no-op for now —
// projection on remote handlers is out of scope for 4a; see Ctx interface
// docs).
var _ interface{ SetProjection(p *projection.Store) } = (*remoteSub)(nil)

func (s *remoteSub) SetProjection(_ *projection.Store) {}
