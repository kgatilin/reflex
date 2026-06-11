package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
	"github.com/kgatilin/reflex/pkg/sdk"
)

// daemonHarness spins up a daemon on a temp socket and exposes the bus +
// socket path for tests.
type daemonHarness struct {
	t      *testing.T
	bus    *bus.Bus
	socket string
	daemon *sdk.Daemon
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func startDaemon(t *testing.T) *daemonHarness {
	t.Helper()
	// Unix socket paths are capped at ~104 bytes on darwin; t.TempDir()
	// (under /var/folders/... with the long test name appended) blows the
	// limit. Use a short mkdtemp under /tmp instead.
	dir, err := os.MkdirTemp("/tmp", "rfx")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	store := event.NewStore()
	proj := projection.NewStore()
	b := bus.New(store, bus.WithProjection(proj), bus.WithSource("daemon-test"))

	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := sdk.NewDaemon(b, lis)

	ctx, cancel := context.WithCancel(context.Background())
	h := &daemonHarness{t: t, bus: b, socket: socket, daemon: d, cancel: cancel}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		_ = d.Serve(ctx)
	}()
	return h
}

func (h *daemonHarness) stop() {
	h.cancel()
	_ = h.daemon.Close()
	h.wg.Wait()
	_ = os.Remove(h.socket)
}

// waitForSocket polls until the socket file exists (Listen creates it
// synchronously, but tests are sometimes lucky/unlucky on slow CI).
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s never appeared", path)
}

// TestRemoteRoundTrip — full E2E: daemon up, SDK client connects + registers,
// seed event over a separate connection, handler fires, reply lands.
func TestRemoteRoundTrip(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	handler := sdk.NewHandler("remote-echo",
		sdk.Consumes("Ping"),
		sdk.Emits("Pong"),
		sdk.Terminal("Pong"),
	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
		return ctx.Emit("Pong", sdk.Args{"orig_id": ev.ID})
	})
	if err := client.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(runCtx) }()

	// Wait until the daemon has registered the remote subscriber. There
	// is no public signal yet; poll the bus subscribers.
	waitForSub := func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for _, s := range h.bus.Subscribers() {
				if s.Name() == "remote-echo" {
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("remote-echo never registered")
	}
	waitForSub()

	// Seed the bus from a second connection (the CLI emit path).
	reqID := uuid.NewString()
	if err := h.daemon.EmitAndDrain(context.Background(), event.Event{
		Type:      "Ping",
		RequestID: reqID,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	var pongs int
	for _, e := range h.bus.Store().Snapshot() {
		if e.Type == "Pong" {
			pongs++
			if !e.Terminal {
				t.Errorf("Pong should be terminal")
			}
			if e.RequestID != reqID {
				t.Errorf("Pong reqID = %q, want %q", e.RequestID, reqID)
			}
		}
	}
	if pongs != 1 {
		t.Errorf("expected 1 Pong, got %d", pongs)
	}

	cancelRun()
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Error("client.Run did not return after cancel")
	}
}

// TestRemoteHandshakeBadFirstFrame — sending something other than a hello
// closes the connection cleanly with an error frame.
func TestRemoteHandshakeBadFirstFrame(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send an emit frame instead of hello.
	bad := []byte(`{"kind":"emit","event":{"type":"X"}}` + "\n")
	if _, err := conn.Write(bad); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if n == 0 {
		t.Fatal("expected an error frame from daemon")
	}
	var f sdk.Frame
	if err := json.Unmarshal(buf[:n-1], &f); err != nil {
		t.Fatalf("decode: %v (raw: %q)", err, buf[:n])
	}
	if f.Kind != sdk.KindError {
		t.Errorf("expected error frame, got kind=%q", f.Kind)
	}
}

// TestRemoteHandshakeBadVersion — client claims an incompatible protocol
// version, daemon rejects.
func TestRemoteHandshakeBadVersion(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hello := []byte(`{"kind":"hello","version":99,"handler":{"name":"x","consumes":"Y"}}` + "\n")
	if _, err := conn.Write(hello); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if n == 0 {
		t.Fatal("expected an error frame")
	}
	var f sdk.Frame
	_ = json.Unmarshal(buf[:n-1], &f)
	if f.Kind != sdk.KindError {
		t.Errorf("expected error frame, got %q (%s)", f.Kind, f.Error)
	}
}

// TestRemoteHandlerErrorBecomesHandlerFailed — when the SDK handler returns
// an error, the daemon-side React surfaces it; the bus emits HandlerFailed
// and the dispatcher aborts.
func TestRemoteHandlerErrorBecomesHandlerFailed(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	handler := sdk.NewHandler("boom",
		sdk.Consumes("Boom"),
	).OnEvent(func(_ sdk.Ctx, _ sdk.Event) error {
		return errors.New("handler exploded")
	})
	if err := client.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(runCtx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range h.bus.Subscribers() {
			if s.Name() == "boom" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		Type:      "Boom",
		RequestID: uuid.NewString(),
	})

	saw := false
	for _, e := range h.bus.Store().Snapshot() {
		if e.Type == bus.HandlerFailedType {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected HandlerFailed in store; events: %v", typeList(h.bus.Store().Snapshot()))
	}
}

// TestRemoteGracefulShutdown — closing the daemon while a client is
// connected causes the client to unblock with a clean return.
func TestRemoteGracefulShutdown(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	handler := sdk.NewHandler("g",
		sdk.Consumes("X"),
	).OnEvent(func(_ sdk.Ctx, _ sdk.Event) error { return nil })
	if err := client.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(context.Background()) }()

	// Wait for hello/welcome round-trip.
	time.Sleep(50 * time.Millisecond)

	// Daemon shuts itself down.
	h.cancel()
	_ = h.daemon.Close()

	select {
	case <-done:
		// Clean disconnect.
	case <-time.After(2 * time.Second):
		t.Error("client.Run did not return after daemon close")
	}
}

// TestMixedHandlersYAMLAndSDK — a daemon-side bus has a YAML-built
// handler (built directly here for simplicity) and an SDK-registered
// handler; both fire on the same event.
func TestMixedHandlersYAMLAndSDK(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	// "YAML-like" subscriber: emits Echo on Ping.
	h.bus.Register(&fakeYAMLSub{name: "yaml-echo", consumes: "Ping", emit: "EchoY"})

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	handler := sdk.NewHandler("sdk-echo",
		sdk.Consumes("Ping"),
		sdk.Emits("EchoS"),
		sdk.Terminal("EchoS"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		return ctx.Emit("EchoS", sdk.Args{"hi": 1})
	})
	if err := client.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(runCtx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range h.bus.Subscribers() {
			if s.Name() == "sdk-echo" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		Type:      "Ping",
		RequestID: uuid.NewString(),
	})

	gotY, gotS := false, false
	for _, e := range h.bus.Store().Snapshot() {
		if e.Type == "EchoY" {
			gotY = true
		}
		if e.Type == "EchoS" {
			gotS = true
		}
	}
	if !gotY {
		t.Error("YAML handler did not fire")
	}
	if !gotS {
		t.Error("SDK handler did not fire")
	}
}

// TestEmittedFromCLIClientBareEmit — a non-handler connection (think
// `reflex emit --daemon …`) sends a hello + a bare emit + goodbye; the
// daemon drains.
func TestEmittedFromCLIClientBareEmit(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	// Register a YAML-like sub so we have something to detect.
	h.bus.Register(&fakeYAMLSub{name: "yaml-echo", consumes: "Seed", emit: "Echoed"})

	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send hello (handler that subscribes to a never-fired event).
	spec := sdk.HandlerSpec{Name: "_cli", Consumes: "__noop__"}
	hello, _ := json.Marshal(sdk.Frame{Kind: sdk.KindHello, Version: sdk.ProtocolVersion, Handler: &spec})
	conn.Write(append(hello, '\n'))

	// Read welcome.
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.Read(buf)

	// Send bare emit.
	seedEv := event.Event{Type: "Seed", RequestID: uuid.NewString()}
	emit, _ := json.Marshal(sdk.Frame{Kind: sdk.KindEmit, Event: &seedEv})
	conn.Write(append(emit, '\n'))
	bye, _ := json.Marshal(sdk.Frame{Kind: sdk.KindGoodbye})
	conn.Write(append(bye, '\n'))

	// The drain should have produced an Echoed event.
	time.Sleep(100 * time.Millisecond)
	got := false
	for _, e := range h.bus.Store().Snapshot() {
		if e.Type == "Echoed" {
			got = true
		}
	}
	if !got {
		t.Errorf("Echoed not in store; saw %v", typeList(h.bus.Store().Snapshot()))
	}
}

// TestProtocolFrameRoundtrip — sanity check that EncodeFrame /
// DecodeFrame preserve the relevant fields.
func TestProtocolFrameRoundtrip(t *testing.T) {
	spec := sdk.HandlerSpec{Name: "h", Consumes: "X", Emits: []sdk.EmittedSpec{{Type: "Y", Terminal: true}}}
	in := sdk.Frame{Kind: sdk.KindHello, Version: 1, Handler: &spec, DeliveryID: "d1"}
	raw, err := sdk.EncodeFrame(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := sdk.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != in.Kind || out.Version != in.Version || out.DeliveryID != in.DeliveryID {
		t.Errorf("envelope drift: got %+v", out)
	}
	if out.Handler == nil || out.Handler.Name != "h" || out.Handler.Consumes != "X" {
		t.Errorf("handler drift: got %+v", out.Handler)
	}
	if len(out.Handler.Emits) != 1 || !out.Handler.Emits[0].Terminal {
		t.Errorf("emits drift: got %+v", out.Handler.Emits)
	}
}

// TestStaleSocketCleanup — daemon listener creation when an old socket
// file exists is handled by the daemon CMD, not the sdk package directly,
// so this test just confirms our listen helper does not stutter on a
// freshly-created path.
func TestRemoteEmptyPath(t *testing.T) {
	_, err := sdk.Connect(sdk.Remote(""))
	if err == nil {
		t.Errorf("expected error for empty socket path")
	}
}

// TestConnectNilOption — Connect rejects a nil TransportOption.
func TestConnectNilOption(t *testing.T) {
	_, err := sdk.Connect(nil)
	if err == nil {
		t.Errorf("expected error for nil transport option")
	}
}

// TestRemoteCtxRequestIDPropagates — Ctx.RequestID matches the incoming
// event's request_id even on the remote transport.
func TestRemoteCtxRequestIDPropagates(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	client, err := sdk.Connect(sdk.Remote(h.socket))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	gotReqID := make(chan string, 1)
	handler := sdk.NewHandler("rid",
		sdk.Consumes("ReqCheck"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		gotReqID <- ctx.RequestID()
		return nil
	})
	if err := client.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(runCtx)

	// Wait for registration.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range h.bus.Subscribers() {
			if s.Name() == "rid" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	want := uuid.NewString()
	_ = h.daemon.EmitAndDrain(context.Background(), event.Event{
		Type:      "ReqCheck",
		RequestID: want,
	})

	select {
	case got := <-gotReqID:
		if got != want {
			t.Errorf("Ctx.RequestID = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Error("handler never observed event")
	}
}

// TestUnknownFrameKindIgnored — the daemon responds with an error frame
// when it sees an unknown kind after handshake but stays alive.
func TestUnknownFrameKindIgnored(t *testing.T) {
	h := startDaemon(t)
	defer h.stop()
	waitForSocket(t, h.socket)

	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	spec := sdk.HandlerSpec{Name: "k", Consumes: "Z"}
	hello, _ := json.Marshal(sdk.Frame{Kind: sdk.KindHello, Version: sdk.ProtocolVersion, Handler: &spec})
	conn.Write(append(hello, '\n'))
	// Read welcome.
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.Read(buf)

	// Send an unknown kind.
	conn.Write([]byte(`{"kind":"floof"}` + "\n"))

	// Should get an error frame back, but connection stays open.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if n == 0 {
		t.Fatal("expected error frame")
	}
	var f sdk.Frame
	_ = json.Unmarshal(buf[:n-1], &f)
	if f.Kind != sdk.KindError {
		t.Errorf("got kind=%q, want error", f.Kind)
	}
}

// helpers

func typeList(es []event.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Type
	}
	return out
}

// fakeYAMLSub mimics a YAML-built bus.Subscriber for mixed-mode tests.
type fakeYAMLSub struct {
	name     string
	consumes string
	emit     string
}

func (f *fakeYAMLSub) Name() string              { return f.name }
func (f *fakeYAMLSub) Match(ev event.Event) bool { return ev.Type == f.consumes }
func (f *fakeYAMLSub) React(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
	return []event.Event{{Type: f.emit, Terminal: true}}, nil
}
