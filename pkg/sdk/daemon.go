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

	mu         sync.Mutex
	conns      map[*conn]struct{}
	closed     bool
	emitMu     sync.Mutex // serialises bus.Run across remote emit() requests
	nextDID    atomic.Uint64
	quiescence QuiescenceFn
}

// NewDaemon constructs a daemon over the supplied bus. Call Serve(ctx) to
// run the accept loop. Close to release the listener and disconnect clients.
func NewDaemon(b *bus.Bus, listener net.Listener) *Daemon {
	return &Daemon{
		bus:      b,
		listener: listener,
		conns:    map[*conn]struct{}{},
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
	conns := make([]*conn, 0, len(d.conns))
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

// QuiescenceFn is the post-drain check the daemon runs after each
// EmitAndDrain. The handler package's CheckQuiescence is the canonical
// implementation; we accept a func so the daemon doesn't have to import
// pkg/handler (which would create a layering cycle: handler depends on
// bus, sdk depends on bus, daemon-using-handler would close the loop).
//
// The daemon ctor sets this from the cmd layer where both packages are
// already in scope.
type QuiescenceFn func(ctx context.Context, b *bus.Bus) error

// SetQuiescence installs the post-drain quiescence check the daemon
// runs after every successful EmitAndDrain. Passing nil disables it.
// Phase 4b: this is the daemon-side counterpart of the in-process
// CheckQuiescence call made by runtime.Run / executeRunWithConfig.
func (d *Daemon) SetQuiescence(fn QuiescenceFn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.quiescence = fn
}

// EmitAndDrain takes a seed event from a CLI client (over the socket) and
// drains the bus. Serialised so multiple concurrent CLI clients don't
// trample on each other's drain.
//
// Phase 4b: after drain, the quiescence check (if installed) runs to
// surface RequestUnhandled / EventOrphaned diagnostics the same way
// `reflex run` does. Failures are propagated as the EmitAndDrain error.
func (d *Daemon) EmitAndDrain(ctx context.Context, seed event.Event) error {
	d.emitMu.Lock()
	defer d.emitMu.Unlock()
	if err := d.bus.Run(ctx, seed); err != nil {
		return err
	}
	d.mu.Lock()
	q := d.quiescence
	d.mu.Unlock()
	if q != nil {
		return q(ctx, d.bus)
	}
	return nil
}

// conn wraps a single client connection. Phase 4b: one connection can
// host N handlers; the per-connection state is shared (encoder, reader,
// awaits, projection RPCs) while each handler keeps its own pending
// delivery table.
type conn struct {
	daemon *Daemon
	net    net.Conn
	encMu  sync.Mutex
	enc    *json.Encoder

	mu         sync.Mutex
	handlers   []*remoteSub
	awaits     []*pendingAwait
	helloSeen  bool

	shutdownOnce sync.Once
	dead         chan struct{}
}

func newConn(d *Daemon, netConn net.Conn) *conn {
	return &conn{
		daemon: d,
		net:    netConn,
		enc:    json.NewEncoder(netConn),
		dead:   make(chan struct{}),
	}
}

func (c *conn) helloPassed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.helloSeen
}

func (c *conn) shutdown() {
	c.shutdownOnce.Do(func() {
		close(c.dead)
		_ = c.net.Close()
		c.mu.Lock()
		hs := c.handlers
		c.mu.Unlock()
		for _, h := range hs {
			h.shutdown()
		}
	})
}

// writeFrame serialises one frame on the wire, holding the encoder mutex
// so concurrent deliver / proj_value / resolved emissions don't interleave.
func (c *conn) writeFrame(f Frame) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.enc.Encode(f)
}

// pendingAwait tracks an in-flight wait-predicate the daemon is checking
// on behalf of a CLI client. The daemon evaluates it after each
// EmitAndDrain; once true, it sends KindResolved and drops the entry.
type pendingAwait struct {
	awaitID   string
	predicate string
	requestID string
}

func (d *Daemon) handleConn(ctx context.Context, netConn net.Conn) {
	c := newConn(d, netConn)
	defer c.shutdown()

	reader := bufio.NewReaderSize(netConn, 1<<20)

	d.mu.Lock()
	d.conns[c] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.conns, c)
		d.mu.Unlock()
	}()

	// Per-connection inbound loop. Multiple hellos are accepted —
	// each register a fresh handler against the same socket.
	for {
		line, err := readLine(reader)
		if err != nil {
			return
		}
		f, err := DecodeFrame(line)
		if err != nil {
			_ = c.writeFrame(Frame{Kind: KindError, Error: fmt.Sprintf("decode: %v", err)})
			return
		}
		// Phase 4a back-compat: the first frame must be a Hello. After
		// the initial Hello, additional Hellos are accepted (multi-handler).
		if !c.helloPassed() && f.Kind != KindHello {
			_ = c.writeFrame(Frame{Kind: KindError, Error: "expected hello"})
			return
		}

		switch f.Kind {
		case KindHello:
			if err := c.handleHello(ctx, f); err != nil {
				_ = c.writeFrame(Frame{Kind: KindError, Error: err.Error()})
				return
			}
			c.mu.Lock()
			c.helloSeen = true
			c.mu.Unlock()
		case KindAck:
			c.completeDelivery(f.DeliveryID, nil)
		case KindNack:
			c.completeDelivery(f.DeliveryID, errors.New(f.Error))
		case KindEmit:
			if f.Event == nil {
				_ = c.writeFrame(Frame{Kind: KindError, Error: "emit missing event"})
				continue
			}
			if f.DeliveryID != "" {
				if !c.addEmit(f.DeliveryID, *f.Event) {
					_ = c.writeFrame(Frame{Kind: KindError, Error: fmt.Sprintf("emit: unknown delivery_id %q", f.DeliveryID)})
				}
				continue
			}
			ev := *f.Event
			if ev.RequestID == "" {
				ev.RequestID = uuid.NewString()
			}
			if err := d.EmitAndDrain(ctx, ev); err != nil {
				_ = c.writeFrame(Frame{Kind: KindError, Error: err.Error()})
				continue
			}
			// After every drain, evaluate any pending awaits.
			c.resolveAwaits()
		case KindAwait:
			c.registerAwait(f)
			// Try immediate resolution in case the predicate is already
			// true (e.g. an earlier drain produced the awaited state).
			c.resolveAwaits()
		case KindProjGet:
			c.handleProjGet(f)
		case KindProjSet:
			c.handleProjSet(f)
		case KindGoodbye:
			return
		default:
			_ = c.writeFrame(Frame{Kind: KindError, Error: fmt.Sprintf("unknown kind %q", f.Kind)})
		}
	}
}

func (c *conn) handleHello(ctx context.Context, f Frame) error {
	_ = ctx
	if f.Handler == nil {
		return errors.New("hello missing handler")
	}
	if f.Version != 0 && f.Version != ProtocolVersion {
		return fmt.Errorf("protocol version %d, expected %d", f.Version, ProtocolVersion)
	}
	if f.Handler.Name == "" || f.Handler.Consumes == "" {
		return errors.New("handler.name and handler.consumes are required")
	}
	sub := newRemoteSub(c.daemon, c, *f.Handler)
	c.daemon.bus.Register(sub)
	c.mu.Lock()
	c.handlers = append(c.handlers, sub)
	c.mu.Unlock()
	return c.writeFrame(Frame{Kind: KindWelcome, HandlerName: sub.spec.Name, Version: ProtocolVersion})
}

// completeDelivery routes an ack/nack to the handler that issued the
// delivery. Each handler's delivery_id space is independent, so we walk
// the slice.
func (c *conn) completeDelivery(deliveryID string, herr error) {
	c.mu.Lock()
	hs := append([]*remoteSub(nil), c.handlers...)
	c.mu.Unlock()
	for _, h := range hs {
		if h.complete(deliveryID, herr) {
			return
		}
	}
}

func (c *conn) addEmit(deliveryID string, ev event.Event) bool {
	c.mu.Lock()
	hs := append([]*remoteSub(nil), c.handlers...)
	c.mu.Unlock()
	for _, h := range hs {
		if h.addEmit(deliveryID, ev) {
			return true
		}
	}
	return false
}

func (c *conn) registerAwait(f Frame) {
	c.mu.Lock()
	c.awaits = append(c.awaits, &pendingAwait{
		awaitID:   f.AwaitID,
		predicate: f.Predicate,
		requestID: f.RequestID,
	})
	c.mu.Unlock()
}

// resolveAwaits walks the pending await list and emits Resolved for any
// predicate that has now become true. Entries that resolve are removed
// from the list.
func (c *conn) resolveAwaits() {
	c.mu.Lock()
	awaits := append([]*pendingAwait(nil), c.awaits...)
	c.mu.Unlock()
	if len(awaits) == 0 {
		return
	}
	store := c.daemon.bus.Store()
	proj := c.daemon.bus.Projection()
	keep := make([]*pendingAwait, 0, len(awaits))
	for _, a := range awaits {
		ok, _ := evalDaemonPredicate(a.predicate, a.requestID, store, proj)
		if ok {
			_ = c.writeFrame(Frame{
				Kind:      KindResolved,
				AwaitID:   a.awaitID,
				Predicate: a.predicate,
				RequestID: a.requestID,
			})
			continue
		}
		keep = append(keep, a)
	}
	c.mu.Lock()
	c.awaits = keep
	c.mu.Unlock()
}

// handleProjGet looks up the key in the daemon's projection store and
// replies with proj_value.
func (c *conn) handleProjGet(f Frame) {
	proj := c.daemon.bus.Projection()
	resp := Frame{Kind: KindProjValue, RPCID: f.RPCID, Key: f.Key, RequestID: f.RequestID}
	if proj != nil {
		if v, ok := proj.Get(f.RequestID, f.Key); ok {
			b, err := json.Marshal(v)
			if err == nil {
				resp.Value = b
				resp.Found = true
			}
		}
	}
	_ = c.writeFrame(resp)
}

// handleProjSet writes the key to the daemon's projection store. The Set
// is fire-and-forget (no response frame); errors in JSON decode are
// reported via KindError because the wire shape was wrong.
func (c *conn) handleProjSet(f Frame) {
	proj := c.daemon.bus.Projection()
	if proj == nil {
		return
	}
	var v any
	if len(f.Value) > 0 {
		if err := json.Unmarshal(f.Value, &v); err != nil {
			_ = c.writeFrame(Frame{Kind: KindError, Error: fmt.Sprintf("proj_set decode: %v", err)})
			return
		}
	}
	proj.Set(f.RequestID, f.Key, v)
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
// connected remote handler. Phase 4b: multiple remoteSubs share a single
// conn so the connection can host N handlers.
type remoteSub struct {
	daemon *Daemon
	spec   HandlerSpec
	conn   *conn

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

func newRemoteSub(d *Daemon, c *conn, spec HandlerSpec) *remoteSub {
	return &remoteSub{
		daemon:  d,
		spec:    spec,
		conn:    c,
		pending: map[string]*pendingDelivery{},
		dead:    make(chan struct{}),
	}
}

func (s *remoteSub) Name() string { return s.spec.Name }
func (s *remoteSub) Match(ev event.Event) bool {
	return ev.Type == s.spec.Consumes
}

// Descriptor lets the bus emit HandlerRegistered + Subscribed control-plane
// events for handlers arriving over the socket.
func (s *remoteSub) Descriptor() bus.HandlerDescriptor {
	d := bus.HandlerDescriptor{Name: s.spec.Name, Consumes: s.spec.Consumes}
	for _, e := range s.spec.Emits {
		d.Emits = append(d.Emits, bus.EmittedDescriptor{
			Type:     e.Type,
			Terminal: e.Terminal,
			Optional: e.Optional,
		})
	}
	return d
}

// React sends Deliver to the remote handler, blocks waiting for Ack/Nack,
// then returns success/error. Emit frames received with the same delivery_id
// before the ack are collected and returned to the dispatcher as if the
// handler had run in-process — preserving the YAML-handler semantics.
func (s *remoteSub) React(ctx context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
	deliveryID := fmt.Sprintf("%s-%d", s.spec.Name, s.daemon.nextDID.Add(1))
	pd := &pendingDelivery{done: make(chan error, 1)}
	s.mu.Lock()
	s.pending[deliveryID] = pd
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, deliveryID)
		s.mu.Unlock()
	}()

	// Frame carries handler_name so the multi-handler client can route
	// the deliver to the right callback. The deliver frame's
	// handler_name field is informational on the daemon side but required
	// on the client for the demultiplex.
	frame := Frame{
		Kind:        KindDeliver,
		DeliveryID:  deliveryID,
		HandlerName: s.spec.Name,
		Event:       &ev,
	}
	if err := s.conn.writeFrame(frame); err != nil {
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

// complete reports completion of a delivery. Returns true when the
// delivery_id matched a pending entry on this handler; false otherwise
// (the caller walks other handlers in the same connection).
func (s *remoteSub) complete(deliveryID string, herr error) bool {
	s.mu.Lock()
	pd, ok := s.pending[deliveryID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case pd.done <- herr:
	default:
	}
	return true
}

func (s *remoteSub) shutdown() {
	s.shutdown1.Do(func() {
		close(s.dead)
	})
}

// Ensure remoteSub satisfies bus.ProjectionAware (cheap no-op for now —
// projection on remote handlers is out of scope for 4a; see Ctx interface
// docs).
var _ interface{ SetProjection(p *projection.Store) } = (*remoteSub)(nil)

func (s *remoteSub) SetProjection(_ *projection.Store) {}

// evalDaemonPredicate is the daemon-side mirror of the CLI's
// checkWaitPredicate. Returns (ok, reason). The supported predicates
// mirror the in-process set:
//
//   - "drain": DrainQuiesced for requestID has fired.
//   - "request_id_terminal": a user-domain terminal event for requestID has
//     fired (RequestHandled / RequestUnhandled / EventOrphaned /
//     LoopExhausted — domain terminals; meta terminals
//     EventDispatched/DrainQuiesced/HandlerFailed don't count).
//   - "projection.has=<key>": projection store has key for requestID.
func evalDaemonPredicate(predicate, requestID string, store *event.Store, proj *projection.Store) (bool, string) {
	switch {
	case predicate == "drain":
		for _, e := range store.Snapshot() {
			if e.Type == projection.TypeDrainQuiesced && e.RequestID == requestID {
				return true, ""
			}
		}
		return false, "no DrainQuiesced for request_id"
	case predicate == "request_id_terminal":
		for _, e := range store.Snapshot() {
			if e.RequestID != requestID || !e.Terminal {
				continue
			}
			switch e.Type {
			case projection.TypeEventDispatched, projection.TypeDrainQuiesced, projection.TypeHandlerFailed,
				bus.HandlerRegisteredType, bus.SubscribedType, bus.UnsubscribedType,
				bus.HandlerDeregisteredType, bus.SubscriptionRejectedType:
				continue
			}
			return true, ""
		}
		return false, "no user-domain terminal event for request_id"
	}
	const prefix = "projection.has="
	if len(predicate) > len(prefix) && predicate[:len(prefix)] == prefix {
		key := predicate[len(prefix):]
		if key == "" {
			return false, "empty key after projection.has="
		}
		if proj != nil && proj.Has(requestID, key) {
			return true, ""
		}
		return false, fmt.Sprintf("projection key %q absent", key)
	}
	return false, fmt.Sprintf("unknown predicate %q", predicate)
}
