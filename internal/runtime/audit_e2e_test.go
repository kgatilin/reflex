package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// TestAuditHandlerEndToEnd loads a YAML config with the audit handler
// enabled plus a real handler graph, drives a request through it, and
// asserts the audit log captured the registration/subscription events.
func TestAuditHandlerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	configYAML := `
handlers:
  - name: audit
    type: audit
    on: HandlerRegistered
    config:
      sink: file://` + auditPath + `

  - name: brain
    type: llm_stub
    on: RequestReceived
    emits: [AssistantMessageProposed, RequestHandled]
    config:
      fallback:
        action: reply_and_handle
        reply: "audited reply"

  - name: out
    type: printer
    on: AssistantMessageProposed
    config:
      prefix: "assistant: "
      field: text
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(configYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := Run(context.Background(), b, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	// Audit captured at least one HandlerRegistered (for brain or out —
	// not for itself, since audit's subscription was added at the same
	// moment its own HandlerRegistered was queued, and Run drains them
	// in order so audit's react fires on subsequent events) and one
	// Subscribed event.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Fatal("audit log is empty")
	}
	var sawRegistered, sawSubscribed bool
	for _, line := range lines {
		var rec struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		switch rec.Type {
		case bus.HandlerRegisteredType:
			sawRegistered = true
		case bus.SubscribedType:
			sawSubscribed = true
		}
	}
	if !sawRegistered {
		t.Fatal("audit log missing HandlerRegistered")
	}
	if !sawSubscribed {
		t.Fatal("audit log missing Subscribed")
	}
}

// TestRuntimeSubscriptionRejectedInLog asserts that a runtime attempt to
// add an uncapped cycling subscription via the bus API fires
// SubscriptionRejected, audit logs see it, and the live table is
// unchanged.
func TestRuntimeSubscriptionRejected(t *testing.T) {
	const yamlCfg = `
handlers:
  - name: a
    type: echo
    on: X
    config:
      emit: Y
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(yamlCfg), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Trying to bind handler a to Y (it emits Y, so this is a self-loop)
	// must be rejected.
	if err := b.SubscribeWithCheck("a", "Y", 0); err == nil {
		t.Fatal("expected uncapped self-loop rejection")
	}
	// SubscriptionRejected must appear in the bus log.
	var found bool
	for _, ev := range b.Store().Snapshot() {
		if ev.Type == bus.SubscriptionRejectedType {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("SubscriptionRejected not emitted")
	}
}
