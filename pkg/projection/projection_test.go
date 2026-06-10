package projection

import (
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/pkg/event"
)

func mustPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestSessionProjectionFoldsLifecycle(t *testing.T) {
	reqID := "r-1"
	events := []event.Event{
		{ID: "e1", Type: TypeRequestReceived, RequestID: reqID, Payload: mustPayload(t, map[string]string{"payload": "what is 2+2"})},
		{ID: "e2", Type: TypeToolCallProposed, RequestID: reqID, CausedBy: "e1", Payload: mustPayload(t, map[string]string{"tool": "calc", "args": "2+2"})},
		{ID: "e3", Type: TypeToolResultObserved, RequestID: reqID, CausedBy: "e2", Payload: mustPayload(t, map[string]string{"result": "4"})},
		{ID: "e4", Type: TypeAssistantMessageProposed, RequestID: reqID, CausedBy: "e3", Payload: mustPayload(t, map[string]string{"text": "The answer is 4"})},
		{ID: "e5", Type: TypeRequestHandled, RequestID: reqID, CausedBy: "e4"},
		// Foreign request — must be ignored.
		{ID: "x", Type: TypeRequestReceived, RequestID: "other", Payload: mustPayload(t, map[string]string{"payload": "ignored"})},
	}

	state := SessionProjection(events, reqID)
	if state.UserMessage != "what is 2+2" {
		t.Errorf("user message = %q", state.UserMessage)
	}
	if len(state.ToolCalls) != 1 || state.ToolCalls[0].Tool != "calc" {
		t.Errorf("tool calls = %+v", state.ToolCalls)
	}
	if len(state.ToolResults) != 1 || state.ToolResults[0].Result != "4" {
		t.Errorf("tool results = %+v", state.ToolResults)
	}
	if len(state.AssistantOutputs) != 1 || state.AssistantOutputs[0] != "The answer is 4" {
		t.Errorf("assistant outputs = %+v", state.AssistantOutputs)
	}
	if !state.Handled || state.Unhandled {
		t.Errorf("handled=%v unhandled=%v", state.Handled, state.Unhandled)
	}
	if len(state.Events) != 5 {
		t.Errorf("expected 5 events for request, got %d", len(state.Events))
	}
}

func TestSessionProjectionUnhandled(t *testing.T) {
	reqID := "r-2"
	events := []event.Event{
		{ID: "e1", Type: TypeRequestReceived, RequestID: reqID},
		{ID: "e2", Type: TypeRequestUnhandled, RequestID: reqID, CausedBy: "e1", Payload: mustPayload(t, map[string]string{"reason": "no handler matched"})},
	}
	state := SessionProjection(events, reqID)
	if state.Handled {
		t.Fatal("expected not handled")
	}
	if !state.Unhandled || state.UnhandledReason != "no handler matched" {
		t.Fatalf("unhandled=%v reason=%q", state.Unhandled, state.UnhandledReason)
	}
}

func TestLastHelpersEmpty(t *testing.T) {
	s := SessionProjection(nil, "x")
	if _, ok := s.LastToolCall(); ok {
		t.Fatal("LastToolCall on empty should be false")
	}
	if _, ok := s.LastToolResult(); ok {
		t.Fatal("LastToolResult on empty should be false")
	}
}
