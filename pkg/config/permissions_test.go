package config

import (
	"testing"
)

// TestParsePermissionsTopLevel: the top-level permissions: block is
// decoded into File.Permissions with all four axes populated.
func TestParsePermissionsTopLevel(t *testing.T) {
	src := `
handlers:
  - name: brain
    type: llm_stub
    on: RequestReceived
permissions:
  - principal: analytics-tool
    grants:
      mutate: [triage.*, chat.*]
      read: ["*"]
      forbidden: [core.*, system.*]
      meta.grant: [triage.*]
`
	f, err := Parse([]byte(src), known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(f.Permissions) != 1 {
		t.Fatalf("len(Permissions) = %d, want 1", len(f.Permissions))
	}
	p := f.Permissions[0]
	if p.Principal != "analytics-tool" {
		t.Errorf("principal = %q", p.Principal)
	}
	if got := p.Grants.Mutate; len(got) != 2 || got[0] != "triage.*" || got[1] != "chat.*" {
		t.Errorf("mutate = %v", got)
	}
	if got := p.Grants.Read; len(got) != 1 || got[0] != "*" {
		t.Errorf("read = %v", got)
	}
	if got := p.Grants.Forbidden; len(got) != 2 || got[0] != "core.*" || got[1] != "system.*" {
		t.Errorf("forbidden = %v", got)
	}
	if got := p.Grants.MetaGrant; len(got) != 1 || got[0] != "triage.*" {
		t.Errorf("meta.grant = %v", got)
	}
}

// TestParseHandlerInlineScopeAndPermissions: scope and inline
// permissions: blocks attach to the handler entry.
func TestParseHandlerInlineScopeAndPermissions(t *testing.T) {
	src := `
handlers:
  - name: triage-tool
    type: llm_stub
    on: RequestReceived
    scope: triage.classify
    permissions:
      mutate: [triage.*]
      read: ["*"]
`
	f, err := Parse([]byte(src), known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h := f.Handlers[0]
	if h.Scope != "triage.classify" {
		t.Errorf("scope = %q", h.Scope)
	}
	if h.Permissions == nil {
		t.Fatal("inline permissions missing")
	}
	if len(h.Permissions.Mutate) != 1 || h.Permissions.Mutate[0] != "triage.*" {
		t.Errorf("inline mutate = %v", h.Permissions.Mutate)
	}
}

// TestParseMissingPermissionsSectionIsLegal: a config without any
// permissions: stanza is fully backwards compatible.
func TestParseMissingPermissionsSectionIsLegal(t *testing.T) {
	src := `
handlers:
  - name: brain
    type: llm_stub
    on: RequestReceived
`
	f, err := Parse([]byte(src), known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Permissions != nil && len(f.Permissions) != 0 {
		t.Fatalf("expected empty permissions, got %v", f.Permissions)
	}
}
