package runtime

import (
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// TestBuildEmitsControlPlaneSeedEvents asserts that compiling a YAML
// config produces one HandlerRegistered + one Subscribed event per
// declared handler on the bus log. This is the Phase 4b promise: YAML
// loading is a seeded stream of control-plane events.
func TestBuildEmitsControlPlaneSeedEvents(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(calcYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	snap := b.Store().Snapshot()
	// calcYAML has 4 handlers; we expect 4 HandlerRegistered + 4 Subscribed.
	var hrCount, sbCount int
	for _, e := range snap {
		switch e.Type {
		case bus.HandlerRegisteredType:
			hrCount++
		case bus.SubscribedType:
			sbCount++
		}
	}
	if hrCount != 4 {
		t.Fatalf("HandlerRegistered count = %d, want 4", hrCount)
	}
	if sbCount != 4 {
		t.Fatalf("Subscribed count = %d, want 4", sbCount)
	}
}

// TestBuildLiveTableMatchesYAML asserts the live table the bus exposes
// is a faithful projection of the YAML config.
func TestBuildLiveTableMatchesYAML(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(calcYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	descs, subs := b.LiveTable()
	if len(descs) != 4 {
		t.Fatalf("descriptors = %d, want 4", len(descs))
	}
	if len(subs) != 4 {
		t.Fatalf("subscriptions = %d, want 4", len(subs))
	}
	wantPairs := map[string]string{
		"brain":            "RequestReceived",
		"brain-after-tool": "ToolResultObserved",
		"calc-tool":        "ToolCallProposed",
		"out":              "AssistantMessageProposed",
	}
	for _, s := range subs {
		want, ok := wantPairs[s.Handler]
		if !ok {
			t.Fatalf("unexpected subscription handler %q", s.Handler)
		}
		if s.EventType != want {
			t.Fatalf("subscription %s: event = %q, want %q", s.Handler, s.EventType, want)
		}
	}
}

// TestBuildRejectsUncappedCycleViaLiveTable asserts the live-table cycle
// check refuses to return a bus when an uncapped cycle is present, even
// when the YAML pre-check is the first line of defence. We construct
// a config that bypasses the YAML check by relying on the per-instance
// resolver and verify the live-table check still fires.
func TestBuildRejectsUncappedCycle(t *testing.T) {
	const cyclic = `
handlers:
  - name: ping
    type: echo
    on: PongEvent
    config:
      emit: PingEvent
  - name: pong
    type: echo
    on: PingEvent
    config:
      emit: PongEvent
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(cyclic), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = Build(cfg, reg)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %v, want cycle", err)
	}
}
