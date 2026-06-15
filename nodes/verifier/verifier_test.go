package verifier_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/verifier"
)

func react(t *testing.T, config string) []engine.Emit {
	t.Helper()
	r, err := verifier.Factory("v", json.RawMessage(config))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	emits, err := r.React(context.Background(), engine.Event{}, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return emits
}

// TestVerifier_GreenWritesVerifiedStatus proves a passing check (exit 0) writes
// the verified terminal status — completion is a value the verifier sets.
func TestVerifier_GreenWritesVerifiedStatus(t *testing.T) {
	emits := react(t, `{"command":["sh","-c","exit 0"]}`)
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(emits))
	}
	if emits[0].Kind != "state.updated.status" {
		t.Errorf("kind = %q, want state.updated.status", emits[0].Kind)
	}
	var p struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(emits[0].Payload, &p); err != nil || p.Status != "done" {
		t.Errorf("payload = %s, want {\"status\":\"done\"}", emits[0].Payload)
	}
}

// TestVerifier_RedKicksVerifyFailedBackIntoLoop proves a failing check (non-zero
// exit) emits verify.failed (not a done status), carrying the exit code and
// output — the brain gets another turn.
func TestVerifier_RedKicksVerifyFailedBackIntoLoop(t *testing.T) {
	emits := react(t, `{"command":["sh","-c","echo boom >&2; exit 3"]}`)
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(emits))
	}
	if emits[0].Kind != "verify.failed" {
		t.Fatalf("kind = %q, want verify.failed", emits[0].Kind)
	}
	var p struct {
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(emits[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", p.ExitCode)
	}
	if p.Output == "" {
		t.Errorf("output is empty, want the failing command's output")
	}
}

// TestVerifier_LaunchFailureIsRedNotPanic proves a bad command feeds verify.failed
// (an event), never a crash (G3).
func TestVerifier_LaunchFailureIsRedNotPanic(t *testing.T) {
	emits := react(t, `{"command":["this-binary-does-not-exist-reflex"]}`)
	if len(emits) != 1 || emits[0].Kind != "verify.failed" {
		t.Fatalf("emits = %v, want one verify.failed (launch failure is red, not a panic)", emits)
	}
}

// TestVerifier_CustomStatusAndEmits proves the status kind, pass value, and
// failed kind are configurable (the verifier is generic over the spine names).
func TestVerifier_CustomStatusAndEmits(t *testing.T) {
	emits := react(t, `{"command":["true"],"status_kind":"task.verified","pass":"green"}`)
	if len(emits) != 1 || emits[0].Kind != "task.verified" {
		t.Fatalf("emits = %v, want one task.verified", emits)
	}
	var p struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(emits[0].Payload, &p)
	if p.Status != "green" {
		t.Errorf("status = %q, want green", p.Status)
	}
}

// TestVerifier_RequiresCommand proves a config with no command is a clean error.
func TestVerifier_RequiresCommand(t *testing.T) {
	if _, err := verifier.Factory("v", json.RawMessage(`{}`)); err == nil {
		t.Fatal("Factory with no command returned nil, want an error")
	}
}
