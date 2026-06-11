package sdk_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
	"github.com/kgatilin/reflex/pkg/sdk"
)

// newInProcBus builds a bus with a projection store, mirroring what the
// runtime would set up. Used by every in-process test below.
func newInProcBus(t *testing.T) *bus.Bus {
	t.Helper()
	store := event.NewStore()
	proj := projection.NewStore()
	return bus.New(store, bus.WithProjection(proj), bus.WithSource("test"))
}

// TestInProcessRoundTrip — register an SDK handler in-process, emit a seed
// event on the bus, verify the handler fires and its emitted event lands on
// the bus.
func TestInProcessRoundTrip(t *testing.T) {
	b := newInProcBus(t)
	client, err := sdk.Connect(sdk.InProcess(b))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	h := sdk.NewHandler("echo",
		sdk.Consumes("Ping"),
		sdk.Emits("Pong"),
		sdk.Terminal("Pong"),
	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
		return ctx.Emit("Pong", sdk.Args{"orig_id": ev.ID})
	})
	if err := client.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := client.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if err := b.Run(context.Background(), event.Event{
		Type:      "Ping",
		RequestID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("bus.Run: %v", err)
	}

	var pongs int
	for _, e := range b.Store().Snapshot() {
		if e.Type == "Pong" {
			pongs++
			if !e.Terminal {
				t.Errorf("Pong should be terminal (was Terminal=%v)", e.Terminal)
			}
			if e.Source != "echo" {
				t.Errorf("Pong source = %q, want %q", e.Source, "echo")
			}
		}
	}
	if pongs != 1 {
		t.Errorf("expected 1 Pong, got %d", pongs)
	}
}

// TestInProcessProjection — handler writes a projection key; subsequent
// reads via the projection store see it.
func TestInProcessProjection(t *testing.T) {
	b := newInProcBus(t)
	client, err := sdk.Connect(sdk.InProcess(b))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	h := sdk.NewHandler("p",
		sdk.Consumes("Trigger"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		ctx.ProjectionSet("verdict", "ok")
		return nil
	})
	if err := client.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := client.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	reqID := uuid.NewString()
	if err := b.Run(context.Background(), event.Event{
		Type:      "Trigger",
		RequestID: reqID,
	}); err != nil {
		t.Fatalf("bus.Run: %v", err)
	}

	v, ok := b.Projection().Get(reqID, "verdict")
	if !ok || v != "ok" {
		t.Errorf("projection: got %v (%v), want \"ok\"/true", v, ok)
	}
}

// TestInProcessTerminalDeniesEmit — emitting from inside a handler whose
// inbound event was Terminal returns ErrTerminalEvent. (The bus still
// routes terminal events to subscribers; the contract is that those
// subscribers must not spawn descendants.)
func TestInProcessTerminalDeniesEmit(t *testing.T) {
	b := newInProcBus(t)
	client, err := sdk.Connect(sdk.InProcess(b))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var emitErr error
	h := sdk.NewHandler("term",
		sdk.Consumes("TerminalIn"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		emitErr = ctx.Emit("Should_not_appear", sdk.Args{})
		return nil
	})
	if err := client.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := client.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if err := b.Run(context.Background(), event.Event{
		Type:      "TerminalIn",
		RequestID: uuid.NewString(),
		Terminal:  true,
	}); err != nil {
		t.Fatalf("bus.Run: %v", err)
	}

	if emitErr != sdk.ErrTerminalEvent {
		t.Errorf("expected ErrTerminalEvent, got %v", emitErr)
	}
	for _, e := range b.Store().Snapshot() {
		if e.Type == "Should_not_appear" {
			t.Error("emit-denied event leaked to bus")
		}
	}
}

// TestHandlerRequiresOnEvent — registering a handler without OnEvent fails.
func TestHandlerRequiresOnEvent(t *testing.T) {
	b := newInProcBus(t)
	client, err := sdk.Connect(sdk.InProcess(b))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	h := sdk.NewHandler("nope", sdk.Consumes("X"))
	if err := client.Register(h); err == nil {
		t.Errorf("expected register error for handler without OnEvent")
	}
}

// TestHandlerRequiresConsumes — handler without Consumes is rejected.
func TestHandlerRequiresConsumes(t *testing.T) {
	b := newInProcBus(t)
	client, _ := sdk.Connect(sdk.InProcess(b))
	defer client.Close()
	h := sdk.NewHandler("nope").OnEvent(func(_ sdk.Ctx, _ sdk.Event) error { return nil })
	if err := client.Register(h); err == nil {
		t.Errorf("expected error for handler without Consumes")
	}
}

// TestRegisterAfterRunRejected — Register must be called before Run.
func TestRegisterAfterRunRejected(t *testing.T) {
	b := newInProcBus(t)
	client, _ := sdk.Connect(sdk.InProcess(b))
	defer client.Close()
	if err := client.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	h := sdk.NewHandler("late", sdk.Consumes("X")).OnEvent(func(_ sdk.Ctx, _ sdk.Event) error { return nil })
	if err := client.Register(h); err == nil {
		t.Errorf("expected error for Register after Run")
	}
}

// TestEmitRaw — EmitRaw round-trips arbitrary JSON.
func TestEmitRaw(t *testing.T) {
	b := newInProcBus(t)
	client, _ := sdk.Connect(sdk.InProcess(b))
	defer client.Close()

	h := sdk.NewHandler("rawcaster",
		sdk.Consumes("Trig"),
		sdk.Emits("Out"),
	).OnEvent(func(ctx sdk.Ctx, _ sdk.Event) error {
		return ctx.EmitRaw("Out", json.RawMessage(`{"raw":42}`))
	})
	_ = client.Register(h)
	_ = client.Run(context.Background())

	_ = b.Run(context.Background(), event.Event{Type: "Trig", RequestID: uuid.NewString()})

	found := false
	for _, e := range b.Store().Snapshot() {
		if e.Type == "Out" && string(e.Payload) == `{"raw":42}` {
			found = true
		}
	}
	if !found {
		t.Error("Out{raw:42} not found")
	}
}

// TestHandlerErrorPropagates — handler returning an error causes the
// dispatcher to emit HandlerFailed and abort.
func TestHandlerErrorPropagates(t *testing.T) {
	b := newInProcBus(t)
	client, _ := sdk.Connect(sdk.InProcess(b))
	defer client.Close()

	h := sdk.NewHandler("boom",
		sdk.Consumes("Boom"),
	).OnEvent(func(_ sdk.Ctx, _ sdk.Event) error {
		return assertErr("handler exploded")
	})
	_ = client.Register(h)
	_ = client.Run(context.Background())

	err := b.Run(context.Background(), event.Event{Type: "Boom", RequestID: uuid.NewString()})
	if err == nil {
		t.Error("expected bus.Run to surface handler error")
	}
	saw := false
	for _, e := range b.Store().Snapshot() {
		if e.Type == bus.HandlerFailedType {
			saw = true
		}
	}
	if !saw {
		t.Error("HandlerFailed meta-event missing")
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
