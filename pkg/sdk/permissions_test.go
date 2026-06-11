package sdk_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/sdk"
)

// TestSDKWithScopePropagatesToDescriptor: an SDK handler declared with
// WithScope("tools.fs.read") shows up on the bus with that scope, so
// runtime permission checks resolve targets correctly.
func TestSDKWithScopePropagatesToDescriptor(t *testing.T) {
	b := newInProcBus(t)
	client, err := sdk.Connect(sdk.InProcess(b))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	h := sdk.NewHandler("fs-tool",
		sdk.Consumes("RequestReceived"),
		sdk.Emits("Done"),
		sdk.WithScope("tools.fs.read"),
		sdk.WithPermissions(sdk.PermSpec{
			Mutate: []string{"tools.*"},
		}),
	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error { return nil })
	if err := client.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := client.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := b.ScopeOf("fs-tool"); got != "tools.fs.read" {
		t.Fatalf("ScopeOf = %q, want tools.fs.read", got)
	}
	got := b.Permissions().SpecFor("fs-tool")
	if len(got.Mutate) != 1 || got.Mutate[0] != "tools.*" {
		t.Fatalf("inline mutate not applied: %+v", got)
	}
	// HandlerRegistered carries the scope on the wire too.
	for _, e := range b.Store().Snapshot() {
		if e.Type != bus.HandlerRegisteredType {
			continue
		}
		var p struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Name == "fs-tool" && p.Scope != "tools.fs.read" {
			t.Fatalf("HandlerRegistered scope = %q", p.Scope)
		}
	}
	_ = event.Event{}
}

// TestSDKImplicitDefaultGrant: a handler without WithScope/WithPermissions
// still receives the default-zone implicit grant, matching the YAML
// loader's behaviour. Phase 1–4b examples therefore continue to work.
func TestSDKImplicitDefaultGrant(t *testing.T) {
	b := newInProcBus(t)
	client, err := sdk.Connect(sdk.InProcess(b))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	h := sdk.NewHandler("echo",
		sdk.Consumes("Ping"),
		sdk.Emits("Pong"),
	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error { return nil })
	_ = client.Register(h)
	_ = client.Run(context.Background())

	got := b.Permissions().SpecFor("echo")
	if len(got.Mutate) != 1 || got.Mutate[0] != "default.*" {
		t.Fatalf("expected default.* mutate, got %+v", got)
	}
}
