package runtime

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// TestScopedCompressionExample drives the worked example end-to-end:
//
//   - boot grants land before HandlerRegistered events;
//   - analytics-stub (mutate:[triage.*]) succeeds when it asks the bus
//     to subscribe a fictional new binding under triage-target's scope;
//   - feedback-saboteur (no feedback.* grant) is denied with reason
//     "forbidden" when it tries to touch feedback.*.
//
// The example file ships in examples/scoped_compression.yaml; this
// test loads the same YAML so a change to the file that breaks the
// worked example breaks the test.
func TestScopedCompressionExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/scoped_compression.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse(data, reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// analytics-stub asks the bus to add a triage-target binding to
	// "OtherEvent". triage-target's scope is triage.classify;
	// analytics-stub's mutate grants admit triage.*. The call accepts;
	// the binding is recorded; no PermissionDenied fires.
	if err := b.SubscribeAs("analytics-stub", "triage-target", "OtherEvent", 0); err != nil {
		t.Fatalf("analytics-stub legit subscribe rejected: %v", err)
	}

	// feedback-saboteur tries to mutate a feedback-scoped target. The
	// reserved-zone default-deny refuses; PermissionDenied fires with
	// reason=forbidden.
	// We synthesize the target by directly setting the bus scope —
	// in real usage the saboteur would target a handler whose YAML
	// scope was already declared in feedback.*.
	b.SetHandlerScope("feedback-target-stub", "feedback.guidance")
	err = b.SubscribeAs("feedback-saboteur", "feedback-target-stub", "X", 0)
	if err == nil {
		t.Fatal("feedback-saboteur should have been denied")
	}

	// Drive a drain so any pending control-plane events get fanned out
	// (audit reacts on this pass).
	if _, err := Run(context.Background(), b, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify the audit file captured at least one PermissionGranted
	// (from the boot stream) and one PermissionDenied (from the
	// saboteur attempt).
	auditPath := "/tmp/reflex-scoped-audit.jsonl"
	defer os.Remove(auditPath)
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var sawGrant, sawDeny bool
	for _, line := range splitLines(string(raw)) {
		var rec struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		switch rec.Type {
		case bus.PermissionGrantedType:
			sawGrant = true
		case bus.PermissionDeniedType:
			sawDeny = true
		}
	}
	if !sawGrant {
		t.Fatal("audit log missing PermissionGranted")
	}
	if !sawDeny {
		t.Fatal("audit log missing PermissionDenied")
	}
}

// splitLines is a trivial helper. Split on '\n' and drop empties so
// trailing newlines don't yield a phantom record.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
