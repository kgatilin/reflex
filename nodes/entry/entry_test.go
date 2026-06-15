package entry_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/entry"
)

func react(t *testing.T, config string, trigger json.RawMessage) []engine.Emit {
	t.Helper()
	r, err := entry.Factory("e", nil, json.RawMessage(config))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	emits, err := r.React(context.Background(), engine.Event{Payload: trigger}, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return emits
}

// TestEntry_PassesTriggerPayloadThrough proves the default (no fixed payload):
// the trigger's payload flows out unchanged under the configured kind — the
// resolver carrying the task text task.new → request.received.
func TestEntry_PassesTriggerPayloadThrough(t *testing.T) {
	emits := react(t, `{"emit":"request.received"}`, json.RawMessage(`{"task":"fix the bug"}`))
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(emits))
	}
	if emits[0].Kind != "request.received" {
		t.Errorf("kind = %q, want request.received", emits[0].Kind)
	}
	if string(emits[0].Payload) != `{"task":"fix the bug"}` {
		t.Errorf("payload = %s, want the trigger payload passed through", emits[0].Payload)
	}
}

// TestEntry_FixedPayloadOverridesTrigger proves a configured fixed payload is
// emitted regardless of the trigger — e.g. mapping budget exhaustion to a fixed
// failed-status transition.
func TestEntry_FixedPayloadOverridesTrigger(t *testing.T) {
	emits := react(t, `{"emit":"state.updated.status","payload":{"status":"failed"}}`, json.RawMessage(`{"reason":"budget"}`))
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(emits))
	}
	if emits[0].Kind != "state.updated.status" {
		t.Errorf("kind = %q, want state.updated.status", emits[0].Kind)
	}
	if string(emits[0].Payload) != `{"status":"failed"}` {
		t.Errorf("payload = %s, want the fixed payload", emits[0].Payload)
	}
}

// TestEntry_EmptyTriggerYieldsObjectPayload proves a payloadless trigger still
// emits a valid (empty-object) payload, never empty bytes.
func TestEntry_EmptyTriggerYieldsObjectPayload(t *testing.T) {
	emits := react(t, `{"emit":"request.received"}`, nil)
	if len(emits) != 1 || string(emits[0].Payload) != "{}" {
		t.Fatalf("emits = %v, want one request.received with {} payload", emits)
	}
}

// TestEntry_RequiresEmitKind proves a config with no emit kind is a clean error,
// not a body that silently emits nothing.
func TestEntry_RequiresEmitKind(t *testing.T) {
	if _, err := entry.Factory("e", nil, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Factory with no emit kind returned nil, want an error")
	}
}
