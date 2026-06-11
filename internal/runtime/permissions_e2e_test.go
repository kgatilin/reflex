package runtime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// TestBootGrantsLandBeforeHandlerRegistered: PermissionGranted events
// from the top-level permissions: block must appear earlier in the
// store than any HandlerRegistered event. The audit handler (when
// subscribed) sees grants first; the permission table is hot before
// runtime mutations get checked.
func TestBootGrantsLandBeforeHandlerRegistered(t *testing.T) {
	const src = `
permissions:
  - principal: fs-tool
    grants:
      mutate: [tools.*]
      read: ["*"]
handlers:
  - name: fs-tool
    type: llm_stub
    on: RequestReceived
    scope: tools.fs.read
`
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(src), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	snap := b.Store().Snapshot()
	firstGrantIdx := -1
	firstRegIdx := -1
	for i, e := range snap {
		switch e.Type {
		case bus.PermissionGrantedType:
			if firstGrantIdx == -1 {
				firstGrantIdx = i
			}
		case bus.HandlerRegisteredType:
			if firstRegIdx == -1 {
				firstRegIdx = i
			}
		}
	}
	if firstGrantIdx == -1 {
		t.Fatal("no PermissionGranted event")
	}
	if firstRegIdx == -1 {
		t.Fatal("no HandlerRegistered event")
	}
	if firstGrantIdx > firstRegIdx {
		t.Fatalf("PermissionGranted at %d should land before HandlerRegistered at %d",
			firstGrantIdx, firstRegIdx)
	}
}

// TestInlinePermissionsEquivalentToTopLevel: an inline `permissions:`
// block under a handler produces the same runtime permission table as
// a separate top-level entry referring to that handler.
func TestInlinePermissionsEquivalentToTopLevel(t *testing.T) {
	inline := `
handlers:
  - name: fs-tool
    type: llm_stub
    on: RequestReceived
    scope: tools.fs.read
    permissions:
      mutate: [tools.*]
      read: ["*"]
`
	separate := `
permissions:
  - principal: fs-tool
    grants:
      mutate: [tools.*]
      read: ["*"]
handlers:
  - name: fs-tool
    type: llm_stub
    on: RequestReceived
    scope: tools.fs.read
`
	reg := handler.BuiltinRegistry()
	cfgA, err := config.Parse([]byte(inline), reg.Types())
	if err != nil {
		t.Fatalf("Parse inline: %v", err)
	}
	cfgB, err := config.Parse([]byte(separate), reg.Types())
	if err != nil {
		t.Fatalf("Parse separate: %v", err)
	}
	a, _ := Build(cfgA, reg)
	bsep, _ := Build(cfgB, reg)
	pa := a.Permissions().SpecFor("fs-tool")
	pb := bsep.Permissions().SpecFor("fs-tool")
	if len(pa.Mutate) != len(pb.Mutate) || len(pa.Read) != len(pb.Read) {
		t.Fatalf("inline=%+v separate=%+v", pa, pb)
	}
	for i := range pa.Mutate {
		if pa.Mutate[i] != pb.Mutate[i] {
			t.Fatalf("mutate[%d]: inline=%q separate=%q", i, pa.Mutate[i], pb.Mutate[i])
		}
	}
}

// TestRuntimeMutationDeniedOutOfScope: a handler with mutate:[tools.*]
// trying to mutate analytics.* gets PermissionDenied{out_of_scope}.
func TestRuntimeMutationDeniedOutOfScope(t *testing.T) {
	const src = `
permissions:
  - principal: fs-tool
    grants:
      mutate: [tools.*]
  - principal: analytics-tool
    grants:
      mutate: [analytics.*]
handlers:
  - name: fs-tool
    type: llm_stub
    on: RequestReceived
    scope: tools.fs.read
  - name: analytics-tool
    type: llm_stub
    on: ToolResultObserved
    scope: analytics.tally
`
	reg := handler.BuiltinRegistry()
	cfg, _ := config.Parse([]byte(src), reg.Types())
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// fs-tool tries to mutate analytics-tool's scope.
	err = b.SubscribeAs("fs-tool", "analytics-tool", "OtherEvent", 0)
	if err == nil {
		t.Fatal("expected denial")
	}
	denied := false
	for _, e := range b.Store().Snapshot() {
		if e.Type != bus.PermissionDeniedType {
			continue
		}
		var p struct {
			Principal   string `json:"principal"`
			TargetScope string `json:"target_scope"`
			Reason      string `json:"reason"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Principal == "fs-tool" && p.TargetScope == "analytics.tally" && p.Reason == "out_of_scope" {
			denied = true
		}
	}
	if !denied {
		t.Fatal("expected PermissionDenied{fs-tool, analytics.tally, out_of_scope}")
	}
}

// TestRuntimeMutationDeniedForbidden: forbidden entry takes precedence
// over an otherwise matching mutate grant.
func TestRuntimeMutationDeniedForbidden(t *testing.T) {
	const src = `
permissions:
  - principal: broad-tool
    grants:
      mutate: [analytics.*, feedback.*]
      forbidden: [feedback.*]
handlers:
  - name: broad-tool
    type: llm_stub
    on: RequestReceived
    scope: analytics.broad
  - name: feedback-target
    type: printer
    on: AssistantMessageProposed
    scope: feedback.rule
`
	reg := handler.BuiltinRegistry()
	cfg, _ := config.Parse([]byte(src), reg.Types())
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	err = b.SubscribeAs("broad-tool", "feedback-target", "OtherEvent", 0)
	if err == nil {
		t.Fatal("expected forbidden denial")
	}
	found := false
	for _, e := range b.Store().Snapshot() {
		if e.Type != bus.PermissionDeniedType {
			continue
		}
		var p struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Reason == "forbidden" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected forbidden reason")
	}
}

// TestExistingExamplesStillBuild: the calc YAML (no scope, no permissions)
// must continue to Build cleanly under Phase 4c. Backwards compatibility
// is a hard anti-goal.
func TestExistingExamplesStillBuild(t *testing.T) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(calcYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Build(cfg, reg); err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestAuditCapturesPermissionEvents: with audit + permissions both
// configured, the audit handler's sink records PermissionGranted and
// PermissionDenied events alongside the topology events.
func TestAuditCapturesPermissionEvents(t *testing.T) {
	dir := t.TempDir()
	auditPath := dir + "/audit.jsonl"
	src := `
permissions:
  - principal: fs-tool
    grants:
      mutate: [tools.*]
handlers:
  - name: audit
    type: audit
    on: HandlerRegistered
    config:
      sink: file://` + auditPath + `
  - name: fs-tool
    type: llm_stub
    on: RequestReceived
    scope: tools.fs.read
`
	reg := handler.BuiltinRegistry()
	cfg, _ := config.Parse([]byte(src), reg.Types())
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Trigger a denial.
	_ = b.SubscribeAs("fs-tool", "audit", "OtherEvent", 0)
	// Drive a drain so the audit handler fires on pending events.
	_, _ = Run(context.Background(), b, "hi")

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var sawGrant, sawDeny bool
	for _, line := range lines {
		var rec struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(line), &rec)
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

// TestMetaGrantRecursive: principal P with meta.grant:[tools.*] can
// publish PermissionGranted for handlers under tools.*, but NOT under
// analytics.*. The bus emits PermissionDenied for the out-of-scope
// case and refuses to update the table.
func TestMetaGrantRecursive(t *testing.T) {
	const src = `
permissions:
  - principal: delegator
    grants:
      meta.grant: [tools.*]
handlers:
  - name: delegator
    type: llm_stub
    on: RequestReceived
    scope: tools.delegator
`
	reg := handler.BuiltinRegistry()
	cfg, _ := config.Parse([]byte(src), reg.Types())
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Allowed: tools.* falls under meta.grant.
	if err := b.PublishPermissionGranted("delegator", "newcomer",
		bus.PermSpec{Mutate: []string{"tools.x"}}); err != nil {
		t.Fatalf("legit grant rejected: %v", err)
	}
	// Denied: analytics.* is outside meta.grant.
	err = b.PublishPermissionGranted("delegator", "rogue",
		bus.PermSpec{Mutate: []string{"analytics.*"}})
	if err == nil {
		t.Fatal("expected meta-grant authority denial")
	}
	if b.Permissions().CheckMutate("rogue", "analytics.x").Allowed {
		t.Fatal("table should not have been updated")
	}
}
