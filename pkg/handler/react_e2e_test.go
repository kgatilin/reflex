package handler

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// scriptedBackend is a PURE mock of the reasoning node: it ignores the model
// entirely and decides the next action from the running transcript. This is
// the whole point of the react example's testability — the topology is
// exercised end to end with deterministic "model" output, no network.
//
// Policy: count how many tool.calc.result lines are already in the transcript.
//   0 results -> ask for the first product   {"action":"tool","tool":"calc","args":"23*7"}
//   1 result  -> ask for the addition         {"action":"tool","tool":"calc","args":"161+11"}
//   2 results -> answer                        {"action":"final","text":"172"}
type scriptedBackend struct{ calls int }

func (s *scriptedBackend) Generate(_ context.Context, _ LLMGeminiConfig, _, transcript string) (string, error) {
	s.calls++
	n := strings.Count(transcript, "tool.calc.result")
	switch {
	case n == 0:
		return `{"action":"tool","tool":"calc","args":"23*7"}`, nil
	case n == 1:
		return `{"action":"tool","tool":"calc","args":"161+11"}`, nil
	default:
		return `{"action":"final","text":"172"}`, nil
	}
}

// buildReactTopology assembles the examples/react.yaml graph in-process: the
// eight handlers, wired onto a fresh bus, with the loop cap on brain. The
// printer writes to buf so the test can assert what would be printed. Returns
// the bus, store, and printer buffer.
func buildReactTopology(t *testing.T, brainCap int) (*bus.Bus, *event.Store, *bytes.Buffer) {
	t.Helper()
	store := event.NewStore()
	b := bus.New(store, bus.WithLoopCaps(map[string]int{"brain": brainCap}))

	buf := &bytes.Buffer{}
	prevW := SetPrinterOutput(buf)
	t.Cleanup(func() { SetPrinterOutput(prevW) })

	mk := func(cfg config.HandlerConfig) {
		sub, err := BuiltinRegistry().Build(cfg)
		if err != nil {
			t.Fatalf("build %q: %v", cfg.Name, err)
		}
		b.Register(sub)
	}

	mk(config.HandlerConfig{Name: "seed", Type: "echo", On: projection.TypeRequestReceived,
		Config: map[string]any{"emit": "llm.turn"}})
	mk(config.HandlerConfig{Name: "brain", Type: "llm_gemini", On: "llm.turn",
		Config: map[string]any{"project": "p", "location": "global", "model": "m",
			"tools": []any{map[string]any{"name": "calc", "description": "adds"}}}})
	mk(config.HandlerConfig{Name: "decode", Type: "llm_decode", On: TypeLLMCompleted,
		Config: map[string]any{"tools": []any{"calc"}}})
	mk(config.HandlerConfig{Name: "calc", Type: "tool_node", On: "tool.calc.call",
		Config: map[string]any{"tool": "calc"}})
	mk(config.HandlerConfig{Name: "pump-result", Type: "echo", On: "tool.calc.result",
		Config: map[string]any{"emit": "llm.turn"}})
	mk(config.HandlerConfig{Name: "pump-failed", Type: "echo", On: "tool.calc.failed",
		Config: map[string]any{"emit": "llm.turn"}})
	mk(config.HandlerConfig{Name: "out", Type: "printer", On: TypeAssistantMessage,
		Config: map[string]any{"prefix": "assistant: ", "field": "text"}})

	return b, store, buf
}

func countType(store *event.Store, typ string) int {
	n := 0
	for _, e := range store.Snapshot() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func hasType(store *event.Store, typ string) bool {
	return countType(store, typ) > 0
}

// TestReactE2EChainsTwoToolCallsThenAnswers drives the full react topology
// with the scripted backend: two calc calls, then a final answer. Asserts the
// trace ends with RequestHandled, the printer would print "172", exactly three
// llm.completed events occurred, and the loop cap was NOT hit.
func TestReactE2EChainsTwoToolCallsThenAnswers(t *testing.T) {
	sb := &scriptedBackend{}
	prev := SetGeminiBackend(sb)
	t.Cleanup(func() { SetGeminiBackend(prev) })

	b, store, buf := buildReactTopology(t, 6)

	if err := b.Run(context.Background(), event.Event{
		Type: projection.TypeRequestReceived, RequestID: "r",
		Payload: jsonRaw(map[string]string{"payload": "what is 23*7 then +11"}),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatalf("CheckQuiescence: %v", err)
	}

	// Trace must end on a terminal RequestHandled.
	if !hasType(store, projection.TypeRequestHandled) {
		t.Fatal("expected RequestHandled in trace")
	}
	// Three turns: two tool calls + the final answer.
	if got := countType(store, TypeLLMCompleted); got != 3 {
		t.Fatalf("llm.completed count = %d, want 3", got)
	}
	// The loop cap (6) must NOT have been hit.
	if hasType(store, bus.LoopExhaustedType) {
		t.Fatal("LoopExhausted fired but the loop should have finished in 3 turns")
	}
	if hasType(store, projection.TypeRequestUnhandled) {
		t.Fatal("RequestUnhandled should not fire on a handled request")
	}
	// The printer would print the final answer.
	if !strings.Contains(buf.String(), "assistant: 172") {
		t.Fatalf("printer output = %q, want it to contain 'assistant: 172'", buf.String())
	}
	// Sanity: exactly two tool results were observed.
	if got := countType(store, "tool.calc.result"); got != 2 {
		t.Fatalf("tool.calc.result count = %d, want 2", got)
	}
}

// TestReactE2ECapStopsTheLoop caps brain at 2 with the same scripted backend.
// The model would keep wanting a second tool call, but the dispatcher refuses
// to fire brain a third time and emits LoopExhausted instead. The request
// never reaches a final answer.
//
// Note on RequestUnhandled: the projection deliberately treats LoopExhausted
// as a clean close (SessionProjection sets Handled=true on it, see
// projection.go), so CheckQuiescence does NOT additionally emit
// RequestUnhandled here — LoopExhausted IS the terminal that closes the
// capped request. We assert LoopExhausted (the real diagnostic) and the
// absence of a final answer rather than a second RequestUnhandled marker.
func TestReactE2ECapStopsTheLoop(t *testing.T) {
	sb := &scriptedBackend{}
	prev := SetGeminiBackend(sb)
	t.Cleanup(func() { SetGeminiBackend(prev) })

	b, store, _ := buildReactTopology(t, 2)

	if err := b.Run(context.Background(), event.Event{
		Type: projection.TypeRequestReceived, RequestID: "r",
		Payload: jsonRaw(map[string]string{"payload": "what is 23*7 then +11"}),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatalf("CheckQuiescence: %v", err)
	}

	// The cap must have fired LoopExhausted.
	if !hasType(store, bus.LoopExhaustedType) {
		t.Fatal("expected LoopExhausted when brain hits its cap of 2")
	}
	// brain fired exactly twice; the third would-be turn was refused, so we
	// never reached a final answer / RequestHandled-via-decode.
	if got := countType(store, TypeLLMCompleted); got != 2 {
		t.Fatalf("llm.completed count = %d, want 2 (cap)", got)
	}
	if hasType(store, TypeAssistantMessage) {
		t.Fatal("no final answer should be produced when the loop is capped short")
	}
}
