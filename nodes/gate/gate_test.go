package gate_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/gate"
)

func react(t *testing.T, config string, trigger json.RawMessage) []engine.Emit {
	t.Helper()
	r, err := gate.Factory("g", json.RawMessage(config))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	emits, err := r.React(context.Background(), engine.Event{Payload: trigger}, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return emits
}

const termCfg = `{"emit":"request.terminal","field":"status","when":["done","failed"]}`

// TestGate_OpensOnTerminalValue proves a terminal status drives the terminal
// fact, carrying the status payload through.
func TestGate_OpensOnTerminalValue(t *testing.T) {
	emits := react(t, termCfg, json.RawMessage(`{"status":"done"}`))
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1 (gate open)", len(emits))
	}
	if emits[0].Kind != "request.terminal" {
		t.Errorf("kind = %q, want request.terminal", emits[0].Kind)
	}
	if string(emits[0].Payload) != `{"status":"done"}` {
		t.Errorf("payload = %s, want the status passed through", emits[0].Payload)
	}
}

// TestGate_StaysClosedOnNonTerminalValue proves a non-terminal status emits
// nothing — the spine keeps turning, no premature terminal.
func TestGate_StaysClosedOnNonTerminalValue(t *testing.T) {
	if emits := react(t, termCfg, json.RawMessage(`{"status":"working"}`)); len(emits) != 0 {
		t.Fatalf("emits = %v, want none (gate closed on a non-terminal value)", emits)
	}
}

// TestGate_ClosedWhenFieldAbsent proves a payload missing the watched field is
// not terminal (no spurious drive).
func TestGate_ClosedWhenFieldAbsent(t *testing.T) {
	if emits := react(t, termCfg, json.RawMessage(`{"notes":"hmm"}`)); len(emits) != 0 {
		t.Fatalf("emits = %v, want none (field absent)", emits)
	}
}

// TestGate_DefaultsFieldToStatus proves the field defaults to "status".
func TestGate_DefaultsFieldToStatus(t *testing.T) {
	emits := react(t, `{"emit":"request.terminal","when":["failed"]}`, json.RawMessage(`{"status":"failed"}`))
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1 (default field=status)", len(emits))
	}
}

// TestGate_RequiresEmitAndWhen proves missing required config is a clean error.
func TestGate_RequiresEmitAndWhen(t *testing.T) {
	if _, err := gate.Factory("g", json.RawMessage(`{"when":["done"]}`)); err == nil {
		t.Error("Factory with no emit returned nil, want error")
	}
	if _, err := gate.Factory("g", json.RawMessage(`{"emit":"x"}`)); err == nil {
		t.Error("Factory with no when returned nil, want error")
	}
}
