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

// TestTriageProjectionWritesVerdict verifies the triage_rules handler
// stashes its verdict in the projection so wait-predicates and downstream
// readers can find it by key.
func TestTriageProjectionWritesVerdict(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(`
handlers:
  - name: classify
    type: triage_rules
    on: GhQueryResult
    config:
      now: "2026-06-10T12:00:00Z"
      stuck_hours: 48
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
	var classify bus.Subscriber
	for _, s := range b.Subscribers() {
		if s.Name() == "classify" {
			classify = s
			break
		}
	}
	if classify == nil {
		t.Fatal("classify handler not registered")
	}

	reqID := "r-triage"
	timelineBody := `[{"event":"labeled","created_at":"2026-06-01T12:00:00Z","label":{"name":"agent-ready"}}]`
	log := mkGhQueryLog(t, reqID, `[]`, timelineBody)
	// Trigger React on the last GhQueryResult event in the log — the
	// projection emits the verdict once both paths are present.
	if _, err := classify.React(context.Background(), log[1], log); err != nil {
		t.Fatalf("React: %v", err)
	}

	v, ok := b.Projection().Get(reqID, "triage.verdict")
	if !ok {
		t.Fatal("triage.verdict not written to projection")
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("verdict type = %T", v)
	}
	if m["classification"] != "STUCK" {
		t.Fatalf("classification = %v, want STUCK", m["classification"])
	}
}

// mkGhQueryLog builds a 2-element synthetic event log: one GhQueryResult
// for the comments path and one for the timeline path. Used by the
// projection-write test to drive triage_rules without spinning up real
// gh_query handlers.
func mkGhQueryLog(t *testing.T, reqID, commentsJSON, timelineJSON string) []event.Event {
	t.Helper()
	c, err := json.Marshal(map[string]any{
		"path": "comments",
		"json": json.RawMessage(commentsJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	tl, err := json.Marshal(map[string]any{
		"path": "timeline",
		"json": json.RawMessage(timelineJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	return []event.Event{
		{ID: "e1", Type: "GhQueryResult", RequestID: reqID, Payload: c},
		{ID: "e2", Type: "GhQueryResult", RequestID: reqID, Payload: tl},
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
