package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/handler"
)

// aggregateYAML drives a fan-out (3 echoes) → aggregator end-to-end and
// asserts the projection is written by the test handler that observes the
// aggregated event. This exercises:
//   - ProjectionAware wiring in the runtime
//   - aggregator gathering after EventDispatched.subscriber_count
//   - projection accessibility from the Result
const aggregateYAML = `
handlers:
  - name: c1
    type: echo
    on: ClassifyRequested
    config: { emit: Classification }
  - name: c2
    type: echo
    on: ClassifyRequested
    config: { emit: Classification }
  - name: c3
    type: echo
    on: ClassifyRequested
    config: { emit: Classification }
  - name: collect
    type: aggregator
    on: Classification
    config:
      expected_from: ClassifyRequested
      emit: ClassificationsAggregated
  - name: announce
    type: printer
    on: ClassificationsAggregated
    config:
      field: count
  - name: finalize
    type: terminator
    on: ClassificationsAggregated
  - name: watcher
    type: unhandled_watcher
    on: __noop__
`

func TestRuntimeAggregatorFanout(t *testing.T) {
	var buf bytes.Buffer
	prev := handler.SetPrinterOutput(&buf)
	t.Cleanup(func() { handler.SetPrinterOutput(prev) })

	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(aggregateYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := runSeed(b, "ClassifyRequested", map[string]any{"item": "foo"}); err != nil {
		t.Fatalf("runSeed: %v", err)
	}
	got := 0
	for _, e := range b.Store().Snapshot() {
		if e.Type == "ClassificationsAggregated" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("ClassificationsAggregated count = %d, want 1", got)
	}
}

// TestProjectionAccessibleAcrossRun runs a tiny chain and asserts that
// values stashed in the projection store before Run remain readable on
// the Result after Run — exercising the runtime wiring.
func TestProjectionAccessibleAcrossRun(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(`
handlers:
  - name: e1
    type: echo
    on: RequestReceived
    config: { emit: AssistantMessageProposed }
  - name: term
    type: terminator
    on: AssistantMessageProposed
  - name: watcher
    type: unhandled_watcher
    on: __noop__
`), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Pre-seed the projection so we can prove Get works through Result.
	b.Projection().Set("explicit", "hello", "world")

	res, err := Run(context.Background(), b, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Projection == nil {
		t.Fatal("Result.Projection is nil — runtime should expose the store")
	}
	if v, ok := res.Projection.Get("explicit", "hello"); !ok || v != "world" {
		t.Fatalf("pre-seeded projection lost: (%v,%v)", v, ok)
	}
}

// runSeed is a thin helper around bus.Bus.Run for tests that need to
// emit arbitrary seed event types (the production Run helper hard-codes
// RequestReceived).
func runSeed(b *bus.Bus, eventType string, payload map[string]any) error {
	reqID := uuid.NewString()
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	seed := event.Event{
		Type:      eventType,
		RequestID: reqID,
		Source:    "test",
		Payload:   raw,
	}
	if err := b.Run(context.Background(), seed); err != nil {
		return err
	}
	return handler.CheckQuiescence(context.Background(), b)
}
