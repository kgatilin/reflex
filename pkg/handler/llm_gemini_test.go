package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// fakeGeminiBackend records the last invocation and returns canned data. No
// network is ever touched.
type fakeGeminiBackend struct {
	reply  string
	err    error
	gotSys string
	gotTr  string
	calls  int
}

func (f *fakeGeminiBackend) Generate(_ context.Context, _ LLMGeminiConfig, system, transcript string) (string, error) {
	f.calls++
	f.gotSys = system
	f.gotTr = transcript
	return f.reply, f.err
}

func newTestGemini(t *testing.T, on string) bus.Subscriber {
	t.Helper()
	sub, err := newLLMGemini(config.HandlerConfig{
		Name: "brain", Type: "llm_gemini", On: on,
		Config: map[string]any{
			"project": "p", "location": "global", "model": "m",
			"tools": []any{map[string]any{"name": "calc", "description": "adds"}},
		},
	})
	if err != nil {
		t.Fatalf("newLLMGemini: %v", err)
	}
	return sub
}

// TestRenderTranscriptSkipsMetaAndForeignRequests checks the domain-blind
// fold: bus meta-events and events from other requests never reach the model.
func TestRenderTranscriptSkipsMetaAndForeignRequests(t *testing.T) {
	log := []event.Event{
		{Type: "RequestReceived", RequestID: "r", Payload: []byte(`{"payload":"hi"}`)},
		{Type: bus.EventDispatchedType, RequestID: "r", Payload: []byte(`{}`)},  // meta, skip
		{Type: bus.DrainQuiescedType, RequestID: "r"},                           // meta, skip
		{Type: bus.HandlerFailedType, RequestID: "r", Payload: []byte(`{}`)},    // meta, skip
		{Type: bus.LoopExhaustedType, RequestID: "r", Payload: []byte(`{}`)},    // meta, skip
		{Type: "tool.calc.result", RequestID: "other", Payload: []byte(`{}`)},   // foreign, skip
		{Type: "tool.calc.result", RequestID: "r", Payload: []byte(`{"result":"4"}`)},
	}
	got := renderTranscript(log, "r")

	if !strings.Contains(got, "RequestReceived") {
		t.Errorf("transcript missing RequestReceived:\n%s", got)
	}
	if !strings.Contains(got, `tool.calc.result: {"result":"4"}`) {
		t.Errorf("transcript missing the request's tool result:\n%s", got)
	}
	for _, meta := range []string{bus.EventDispatchedType, bus.DrainQuiescedType, bus.HandlerFailedType, bus.LoopExhaustedType} {
		if strings.Contains(got, meta) {
			t.Errorf("transcript leaked meta-event %q:\n%s", meta, got)
		}
	}
	// The foreign request's result must not appear. Only the "r" copy may.
	if strings.Count(got, "tool.calc.result") != 1 {
		t.Errorf("expected exactly one tool.calc.result line, got:\n%s", got)
	}
}

// TestLLMGeminiEmitsCompleted: a mocked backend returning canned JSON yields a
// single non-terminal llm.completed{text} carrying the verbatim reply.
func TestLLMGeminiEmitsCompleted(t *testing.T) {
	fb := &fakeGeminiBackend{reply: `{"action":"final","text":"42"}`}
	prev := SetGeminiBackend(fb)
	t.Cleanup(func() { SetGeminiBackend(prev) })

	sub := newTestGemini(t, "llm.turn")
	ev := event.Event{Type: "llm.turn", RequestID: "r"}
	log := []event.Event{
		{Type: "RequestReceived", RequestID: "r", Payload: []byte(`{"payload":"what is 6*7"}`)},
		ev,
	}
	out, err := sub.React(context.Background(), ev, log)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out))
	}
	if out[0].Type != TypeLLMCompleted {
		t.Fatalf("type = %s, want %s", out[0].Type, TypeLLMCompleted)
	}
	if out[0].Terminal {
		t.Fatal("llm.completed must be non-terminal")
	}
	var p struct {
		Text string `json:"text"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Text != `{"action":"final","text":"42"}` {
		t.Fatalf("text = %q (not forwarded verbatim)", p.Text)
	}
	// The system prompt must advertise the configured tool menu.
	if !strings.Contains(fb.gotSys, "calc") {
		t.Errorf("system prompt missing tool menu:\n%s", fb.gotSys)
	}
	if fb.calls != 1 {
		t.Fatalf("backend called %d times, want exactly 1", fb.calls)
	}
}

// TestLLMGeminiBackendErrorEmitsFailedNotGoError: a backend error becomes a
// non-terminal llm.failed event — NOT a Go error, which would abort the drain.
func TestLLMGeminiBackendErrorEmitsFailedNotGoError(t *testing.T) {
	fb := &fakeGeminiBackend{err: errors.New("vertex unavailable")}
	prev := SetGeminiBackend(fb)
	t.Cleanup(func() { SetGeminiBackend(prev) })

	sub := newTestGemini(t, "llm.turn")
	ev := event.Event{Type: "llm.turn", RequestID: "r"}
	out, err := sub.React(context.Background(), ev, []event.Event{ev})
	if err != nil {
		t.Fatalf("React returned a Go error (aborts the drain): %v", err)
	}
	if len(out) != 1 || out[0].Type != TypeLLMFailed {
		t.Fatalf("expected one llm.failed, got %+v", out)
	}
	if out[0].Terminal {
		t.Fatal("llm.failed must be non-terminal")
	}
	var p struct {
		Error string `json:"error"`
	}
	_ = out[0].PayloadAs(&p)
	if !strings.Contains(p.Error, "vertex unavailable") {
		t.Fatalf("error payload = %q", p.Error)
	}
}

// TestLLMGeminiRequiresProjectAndModel guards the factory's validation.
func TestLLMGeminiRequiresProjectAndModel(t *testing.T) {
	_, err := newLLMGemini(config.HandlerConfig{
		Name: "brain", Type: "llm_gemini", On: "llm.turn",
		Config: map[string]any{"model": "m"}, // no project
	})
	if err == nil {
		t.Fatal("expected error when project missing")
	}
}
