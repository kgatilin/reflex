package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

func TestLLMStubMatchesRule(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "brain",
		Type: "llm_stub",
		On:   projection.TypeRequestReceived,
		Config: map[string]any{
			"rules": []map[string]any{
				{"match": "2+2", "action": "tool_call", "tool": "calc", "args": "2+2"},
			},
			"fallback": map[string]any{
				"action": "reply_and_handle", "reply": "I don't know.",
			},
		},
	}
	sub, err := newLLMStub(cfg)
	if err != nil {
		t.Fatalf("newLLMStub: %v", err)
	}
	store := event.NewStore()
	store.Append(event.Event{
		ID: "e1", Type: projection.TypeRequestReceived, RequestID: "r",
		Payload: jsonRaw(map[string]string{"payload": "what is 2+2"}),
	})
	out, err := sub.React(context.Background(),
		event.Event{ID: "e1", Type: projection.TypeRequestReceived, RequestID: "r"},
		store.Snapshot())
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != projection.TypeToolCallProposed {
		t.Fatalf("got %+v", out)
	}
}

func TestLLMStubFallbackHandles(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "brain", Type: "llm_stub", On: projection.TypeRequestReceived,
		Config: map[string]any{
			"fallback": map[string]any{"action": "reply_and_handle", "reply": "fallback"},
		},
	}
	sub, _ := newLLMStub(cfg)
	out, err := sub.React(context.Background(),
		event.Event{Type: projection.TypeRequestReceived, RequestID: "r"},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 events, got %d", len(out))
	}
	if out[1].Type != projection.TypeRequestHandled {
		t.Fatalf("second event = %s", out[1].Type)
	}
}

func TestToolCallExecutesCalc(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "calc-tool", Type: "tool_call", On: projection.TypeToolCallProposed,
		Config: map[string]any{"tool": "calc"},
	}
	sub, err := newToolCall(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ev := event.Event{
		Type:    projection.TypeToolCallProposed,
		Payload: jsonRaw(map[string]string{"tool": "calc", "args": "2+2"}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out))
	}
	var p struct {
		Result string `json:"result"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Result != "4" {
		t.Fatalf("result = %q", p.Result)
	}
}

func TestPrinterWritesText(t *testing.T) {
	var buf bytes.Buffer
	prev := SetPrinterOutput(&buf)
	t.Cleanup(func() { SetPrinterOutput(prev) })

	cfg := config.HandlerConfig{
		Name: "out", Type: "printer", On: projection.TypeAssistantMessageProposed,
	}
	sub, _ := newPrinter(cfg)
	_, err := sub.React(context.Background(),
		event.Event{
			Type:    projection.TypeAssistantMessageProposed,
			Payload: jsonRaw(map[string]string{"text": "hello"}),
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("buffer = %q", buf.String())
	}
}

func TestCheckQuiescenceFlagsUnhandled(t *testing.T) {
	store := event.NewStore()
	b := bus.New(store)
	b.Emit(event.Event{Type: projection.TypeRequestReceived, RequestID: "r"})
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	hasUnhandled := false
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeRequestUnhandled {
			hasUnhandled = true
		}
	}
	if !hasUnhandled {
		t.Fatal("expected RequestUnhandled in log")
	}
}

func TestCheckQuiescenceSkipsHandled(t *testing.T) {
	store := event.NewStore()
	b := bus.New(store)
	b.Emit(event.Event{Type: projection.TypeRequestReceived, RequestID: "r"})
	b.Emit(event.Event{Type: projection.TypeRequestHandled, RequestID: "r"})
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeRequestUnhandled {
			t.Fatal("should not have emitted RequestUnhandled")
		}
	}
}

func TestBuiltinRegistryHasExpectedTypes(t *testing.T) {
	r := BuiltinRegistry()
	want := []string{
		"llm_stub", "tool_call", "printer", "terminator",
		"unhandled_watcher", "echo",
	}
	got := r.Types()
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing type %q in registry", w)
		}
	}
}

func TestOrphanWatcherFlagsNonTerminalLeaf(t *testing.T) {
	store := event.NewStore()
	b := bus.New(store)
	// A synthetic chain: A (non-terminal) → B (non-terminal, no children).
	// Both are emitted directly; B should be flagged as orphan.
	first := b.Emit(event.Event{Type: "A", RequestID: "r"})
	b.Emit(event.Event{Type: "B", RequestID: "r", CausedBy: first.ID})
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	gotOrphan := false
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeEventOrphaned {
			gotOrphan = true
			if !e.Terminal {
				t.Fatal("EventOrphaned must itself be terminal")
			}
			var p map[string]string
			if err := e.PayloadAs(&p); err != nil {
				t.Fatalf("decode orphan payload: %v", err)
			}
			if p["orphan_type"] != "B" {
				t.Fatalf("orphan_type = %q, want B", p["orphan_type"])
			}
		}
	}
	if !gotOrphan {
		t.Fatal("expected EventOrphaned for leaf B")
	}
}

func TestOrphanWatcherSkipsTerminalLeaf(t *testing.T) {
	store := event.NewStore()
	b := bus.New(store)
	// Chain ending in terminal RequestHandled — must NOT produce orphan.
	first := b.Emit(event.Event{Type: "RequestReceived", RequestID: "r"})
	b.Emit(event.Event{
		Type: projection.TypeRequestHandled, RequestID: "r",
		CausedBy: first.ID, Terminal: true,
	})
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeEventOrphaned {
			t.Fatalf("unexpected EventOrphaned: %+v", e)
		}
	}
}

func TestOrphanWatcherRespectsExplicitTerminal(t *testing.T) {
	// A handler that emits an explicit terminal event should not trip the
	// orphan watcher even if no further descendants follow.
	store := event.NewStore()
	b := bus.New(store)
	first := b.Emit(event.Event{Type: "RequestReceived", RequestID: "r"})
	b.Emit(event.Event{
		Type: "AssistantMessageProposed", RequestID: "r",
		CausedBy: first.ID, Terminal: true,
	})
	b.Emit(event.Event{
		Type: projection.TypeRequestHandled, RequestID: "r",
		CausedBy: first.ID, Terminal: true,
	})
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeEventOrphaned {
			t.Fatalf("unexpected EventOrphaned: %+v", e)
		}
	}
}

func TestOrphanWatcherDoesNotLoopOnItself(t *testing.T) {
	// EventOrphaned itself is terminal — so re-running CheckQuiescence on a
	// log that already contains one must not multiply the orphans.
	store := event.NewStore()
	b := bus.New(store)
	first := b.Emit(event.Event{Type: "X", RequestID: "r"})
	b.Emit(event.Event{Type: "Y", RequestID: "r", CausedBy: first.ID}) // orphan
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	countAfter1 := 0
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeEventOrphaned {
			countAfter1++
		}
	}
	if err := CheckQuiescence(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	countAfter2 := 0
	for _, e := range store.Snapshot() {
		if e.Type == projection.TypeEventOrphaned {
			countAfter2++
		}
	}
	if countAfter1 != countAfter2 {
		t.Fatalf("orphan count changed on second pass: %d → %d", countAfter1, countAfter2)
	}
}

func jsonRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
