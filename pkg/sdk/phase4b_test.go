package sdk_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
	"github.com/kgatilin/reflex/pkg/sdk"
)

// busPkg aliases bus.Bus so the QuiescenceFn type signature compiles
// without importing the unused bus path under a name alias.
type busPkg = bus.Bus

// newLineEnc wraps a writer with one-line-per-frame JSON encoding.
func newLineEnc(w io.Writer) func(sdk.Frame) error {
	enc := json.NewEncoder(w)
	return func(f sdk.Frame) error { return enc.Encode(f) }
}

// newLineDec wraps a reader with one-line-per-frame JSON decoding.
func newLineDec(r io.Reader) func() (sdk.Frame, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	return func() (sdk.Frame, error) {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return sdk.DecodeFrame(line)
			}
			return sdk.Frame{}, err
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		return sdk.DecodeFrame(line)
	}
}

// TestRemoteMultiHandlerOneConnection — Phase 4b B3. One Client registers
// two handlers consuming different event types; both fire correctly via
// the same socket.
func TestRemoteMultiHandlerOneConnection(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var pingCount, pongCount int32

	pingHandler := sdk.NewHandler("ping-watcher",
		sdk.Consumes("Ping"),
	).OnEvent(func(_ sdk.Ctx, _ sdk.Event) error {
		atomic.AddInt32(&pingCount, 1)
		return nil
	})
	pongHandler := sdk.NewHandler("pong-watcher",
		sdk.Consumes("Pong"),
	).OnEvent(func(_ sdk.Ctx, _ sdk.Event) error {
		atomic.AddInt32(&pongCount, 1)
		return nil
	})
	if err := client.Register(pingHandler); err != nil {
		t.Fatalf("register ping: %v", err)
	}
	if err := client.Register(pongHandler); err != nil {
		t.Fatalf("register pong: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan struct{})
	go func() {
		_ = client.Run(runCtx)
		close(clientDone)
	}()

	// Wait until both handlers are visible on the daemon bus.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		names := map[string]bool{}
		for _, s := range h.bus.Subscribers() {
			names[s.Name()] = true
		}
		if names["ping-watcher"] && names["pong-watcher"] {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		ID: uuid.NewString(), Type: "Ping", RequestID: uuid.NewString(),
	})
	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		ID: uuid.NewString(), Type: "Pong", RequestID: uuid.NewString(),
	})

	if got := atomic.LoadInt32(&pingCount); got != 1 {
		t.Errorf("ping fires = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&pongCount); got != 1 {
		t.Errorf("pong fires = %d, want 1", got)
	}
	cancel()
	<-clientDone
}

// TestRemoteProjectionRoundTrip — Phase 4b B2. A remote handler writes via
// ProjectionSet; another remote handler reads via ProjectionGet over the
// daemon's projection store.
func TestRemoteProjectionRoundTrip(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	writer := sdk.NewHandler("writer",
		sdk.Consumes("Write"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		ctx.ProjectionSet("answer", 42)
		return nil
	})
	if err := client.Register(writer); err != nil {
		t.Fatalf("register writer: %v", err)
	}

	var readVal any
	var readOK bool
	reader := sdk.NewHandler("reader",
		sdk.Consumes("Read"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		readVal, readOK = ctx.ProjectionGet("answer")
		return nil
	})
	if err := client.Register(reader); err != nil {
		t.Fatalf("register reader: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan struct{})
	go func() {
		_ = client.Run(runCtx)
		close(clientDone)
	}()

	// Wait until handlers are on the bus.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		names := map[string]bool{}
		for _, s := range h.bus.Subscribers() {
			names[s.Name()] = true
		}
		if names["writer"] && names["reader"] {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	reqID := uuid.NewString()
	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		ID: uuid.NewString(), Type: "Write", RequestID: reqID,
	})
	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		ID: uuid.NewString(), Type: "Read", RequestID: reqID,
	})

	if !readOK {
		t.Fatal("ProjectionGet returned ok=false; expected the stored value")
	}
	// JSON round-trip turns 42 into float64.
	if v, ok := readVal.(float64); !ok || v != 42 {
		t.Fatalf("ProjectionGet value = %v (%T), want 42", readVal, readVal)
	}
	cancel()
	<-clientDone
}

// TestDaemonAwaitDrain — Phase 4b B1. The CLI installs an `await drain`
// frame; the daemon resolves it after the seed event drains.
func TestDaemonAwaitDrain(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	// Register a no-op handler so the bus has a subscriber.
	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	terminator := sdk.NewHandler("term",
		sdk.Consumes("Trigger"),
		sdk.Emits("Done"),
		sdk.Terminal("Done"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		return ctx.Emit("Done", nil)
	})
	if err := client.Register(terminator); err != nil {
		t.Fatalf("register: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan struct{})
	go func() { _ = client.Run(runCtx); close(clientDone) }()

	// Wait for handler to be visible.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range h.bus.Subscribers() {
			if s.Name() == "term" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Open a separate CLI-style conn for the await.
	cliConn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cliConn.Close()
	enc := newLineEnc(cliConn)
	dec := newLineDec(cliConn)
	if err := enc(sdk.Frame{
		Kind:    sdk.KindHello,
		Version: sdk.ProtocolVersion,
		Handler: &sdk.HandlerSpec{Name: "_cli", Consumes: "__noop__"},
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	w, err := dec()
	if err != nil || w.Kind != sdk.KindWelcome {
		t.Fatalf("welcome: %v %+v", err, w)
	}
	reqID := uuid.NewString()
	if err := enc(sdk.Frame{
		Kind:      sdk.KindAwait,
		AwaitID:   "a1",
		Predicate: "drain",
		RequestID: reqID,
	}); err != nil {
		t.Fatalf("await: %v", err)
	}
	if err := enc(sdk.Frame{
		Kind:  sdk.KindEmit,
		Event: &event.Event{Type: "Trigger", RequestID: reqID},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	resolved := make(chan sdk.Frame, 1)
	errCh := make(chan error, 1)
	go func() {
		for {
			f, err := dec()
			if err != nil {
				errCh <- err
				return
			}
			if f.Kind == sdk.KindResolved {
				resolved <- f
				return
			}
		}
	}()
	select {
	case f := <-resolved:
		if f.AwaitID != "a1" {
			t.Fatalf("await_id = %q", f.AwaitID)
		}
	case err := <-errCh:
		t.Fatalf("decode err while waiting for resolved: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("await drain never resolved")
	}
	cancel()
	<-clientDone
}

// TestDaemonAwaitRequestIDTerminal — B1 variant for the user-domain
// terminal predicate.
func TestDaemonAwaitRequestIDTerminal(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	handlerH := sdk.NewHandler("h",
		sdk.Consumes("Go"),
		sdk.Emits("Done"),
		sdk.Terminal("Done"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		return ctx.Emit("Done", nil)
	})
	if err := client.Register(handlerH); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(runCtx) }()

	// Wait for handler to be visible.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range h.bus.Subscribers() {
			if s.Name() == "h" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cli, _ := net.Dial("unix", h.socket)
	defer cli.Close()
	enc := newLineEnc(cli)
	dec := newLineDec(cli)
	_ = enc(sdk.Frame{Kind: sdk.KindHello, Version: sdk.ProtocolVersion, Handler: &sdk.HandlerSpec{Name: "_cli", Consumes: "__noop__"}})
	_, _ = dec()
	reqID := uuid.NewString()
	_ = enc(sdk.Frame{Kind: sdk.KindAwait, AwaitID: "rt", Predicate: "request_id_terminal", RequestID: reqID})
	_ = enc(sdk.Frame{Kind: sdk.KindEmit, Event: &event.Event{Type: "Go", RequestID: reqID}})
	resolved := make(chan sdk.Frame, 1)
	go func() {
		for {
			f, err := dec()
			if err != nil {
				return
			}
			if f.Kind == sdk.KindResolved {
				resolved <- f
				return
			}
		}
	}()
	select {
	case <-resolved:
	case <-time.After(3 * time.Second):
		t.Fatal("request_id_terminal never resolved")
	}
}

// TestDaemonAwaitProjectionHas — B1 variant for the projection predicate.
func TestDaemonAwaitProjectionHas(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	// Pre-seed a projection value so the predicate is true immediately.
	reqID := uuid.NewString()
	h.bus.Projection().Set(reqID, "key", "value")

	cli, _ := net.Dial("unix", h.socket)
	defer cli.Close()
	enc := newLineEnc(cli)
	dec := newLineDec(cli)
	_ = enc(sdk.Frame{Kind: sdk.KindHello, Version: sdk.ProtocolVersion, Handler: &sdk.HandlerSpec{Name: "_cli", Consumes: "__noop__"}})
	_, _ = dec()
	_ = enc(sdk.Frame{Kind: sdk.KindAwait, AwaitID: "p1", Predicate: "projection.has=key", RequestID: reqID})
	// Drain via a no-op emit so the daemon evaluates the await.
	_ = enc(sdk.Frame{Kind: sdk.KindEmit, Event: &event.Event{Type: "__irrelevant__", RequestID: reqID}})
	resolved := make(chan sdk.Frame, 1)
	go func() {
		for {
			f, err := dec()
			if err != nil {
				return
			}
			if f.Kind == sdk.KindResolved {
				resolved <- f
				return
			}
		}
	}()
	select {
	case <-resolved:
	case <-time.After(3 * time.Second):
		t.Fatal("projection.has=key never resolved")
	}
}

// TestDaemonDrainQuiesced — Phase 4b B4 (drain side). A daemon's
// EmitAndDrain triggers the usual DrainQuiesced emission for the
// request_id, just like in-process `reflex run`.
func TestDaemonDrainQuiesced(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	reqID := uuid.NewString()
	if err := h.daemon.EmitAndDrain(context.Background(), event.Event{
		Type: "Unhandled", RequestID: reqID,
	}); err != nil {
		t.Fatalf("EmitAndDrain: %v", err)
	}
	var foundDQ bool
	for _, e := range h.bus.Store().Snapshot() {
		if e.Type == projection.TypeDrainQuiesced && e.RequestID == reqID {
			foundDQ = true
		}
	}
	if !foundDQ {
		t.Fatal("daemon EmitAndDrain did not emit DrainQuiesced")
	}
}

// TestDaemonCheckQuiescenceWiredIn — Phase 4b B4 (quiescence). Once
// SetQuiescence is installed, the daemon emits RequestUnhandled for a
// request that never reached a closing terminal.
func TestDaemonCheckQuiescenceWiredIn(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	// Inline quiescence: emit RequestUnhandled for each request without
	// a closing terminal. This mirrors the real handler.CheckQuiescence
	// without cross-importing the handler package (which would create a
	// test layering cycle).
	h.daemon.SetQuiescence(func(_ context.Context, b *busPkg) error {
		snap := b.Store().Snapshot()
		seen := map[string]bool{}
		for _, e := range snap {
			if e.RequestID == "" || seen[e.RequestID] {
				continue
			}
			seen[e.RequestID] = true
			state := projection.SessionProjection(snap, e.RequestID)
			if state.Handled || state.Unhandled {
				continue
			}
			b.Emit(event.Event{
				Type:      projection.TypeRequestUnhandled,
				RequestID: e.RequestID,
				Source:    "test-quiescence",
				Terminal:  true,
			})
		}
		return nil
	})

	reqID := uuid.NewString()
	if err := h.daemon.EmitAndDrain(context.Background(), event.Event{
		Type: "Stale", RequestID: reqID,
	}); err != nil {
		t.Fatalf("EmitAndDrain: %v", err)
	}
	var sawUnhandled bool
	for _, e := range h.bus.Store().Snapshot() {
		if e.Type == projection.TypeRequestUnhandled && e.RequestID == reqID {
			sawUnhandled = true
		}
	}
	if !sawUnhandled {
		t.Fatal("daemon's quiescence func did not produce RequestUnhandled")
	}
}
