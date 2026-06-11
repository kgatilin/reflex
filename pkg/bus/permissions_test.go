package bus

import (
	"testing"
)

// matchScope is the core matcher. Conservative wildcard: "*" matches
// one or more components, never zero. These tests pin the semantics
// the bus enforcement layer relies on.
func TestMatchScopeExactAndWildcard(t *testing.T) {
	cases := []struct {
		pattern string
		target  string
		want    bool
	}{
		// Exact equality.
		{"tools", "tools", true},
		{"tools.fs.read", "tools.fs.read", true},
		// Exact requires equality — no prefix promotion.
		{"tools", "tools.fs.read", false},
		// Trailing .* matches one or more additional components.
		{"tools.*", "tools.fs.read", true},
		{"tools.*", "tools.fs.read.line", true},
		// Conservative: "tools.*" does NOT match bare "tools".
		{"tools.*", "tools", false},
		// Mismatched prefix.
		{"tools.*", "analytics.foo", false},
		// Bare "*" matches any non-empty target.
		{"*", "tools", true},
		{"*", "analytics.metrics", true},
		{"*", "", false},
		// Empty target never matches.
		{"tools.*", "", false},
		// Dotted prefix sentinel — pattern containing "." but not ending
		// in ".*" must be an exact match.
		{"tools.fs.read", "tools.fs.read.line", false},
	}
	for _, c := range cases {
		got := matchScope(c.pattern, c.target)
		if got != c.want {
			t.Errorf("matchScope(%q, %q) = %v, want %v", c.pattern, c.target, got, c.want)
		}
	}
}

// CheckMutate returns out_of_scope when the principal has no matching
// mutate grant. Default-deny is the bus's posture for unknown principals.
func TestPermissionTableDeniesUnknownPrincipal(t *testing.T) {
	pt := NewPermissionTable()
	d := pt.CheckMutate("rogue", "tools.fs.read")
	if d.Allowed {
		t.Fatalf("unknown principal should be denied")
	}
	if d.Reason != "out_of_scope" {
		t.Fatalf("reason = %q, want out_of_scope", d.Reason)
	}
}

// A matching mutate grant allows the operation.
func TestPermissionTableAllowsMatchingMutate(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("fs-tool", PermSpec{Mutate: []string{"tools.*"}})
	d := pt.CheckMutate("fs-tool", "tools.fs.read")
	if !d.Allowed {
		t.Fatalf("allowed=false reason=%q", d.Reason)
	}
}

// Forbidden overrides a broader mutate grant.
func TestPermissionTableForbiddenOverridesMutate(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("broad", PermSpec{
		Mutate:    []string{"*"},
		Forbidden: []string{"core.*"},
	})
	d := pt.CheckMutate("broad", "core.dispatcher")
	if d.Allowed {
		t.Fatalf("forbidden should deny")
	}
	if d.Reason != "forbidden" {
		t.Fatalf("reason = %q, want forbidden", d.Reason)
	}
	// But a non-core target is still allowed.
	if !pt.CheckMutate("broad", "tools.x").Allowed {
		t.Fatalf("non-core target should pass with mutate:*")
	}
}

// Reserved zones (core.*, system.*, feedback.*) deny by default even
// when the principal has no forbidden entry — they only allow through
// an explicit matching mutate grant.
func TestPermissionTableDeniesReservedZonesByDefault(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("default-handler", PermSpec{Mutate: []string{"default.*"}})
	for _, target := range []string{
		"core.dispatcher", "system.bus", "feedback.guidance",
	} {
		d := pt.CheckMutate("default-handler", target)
		if d.Allowed {
			t.Errorf("reserved zone %q should be denied for default-handler", target)
		}
		if d.Reason != "forbidden" {
			t.Errorf("reserved-zone reason = %q, want forbidden", d.Reason)
		}
	}
}

// An explicit mutate grant lets a principal touch reserved zones.
func TestPermissionTableExplicitGrantAllowsReservedZone(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("system-tool", PermSpec{Mutate: []string{"system.permissions.*"}})
	d := pt.CheckMutate("system-tool", "system.permissions.audit")
	if !d.Allowed {
		t.Fatalf("explicit reserved grant should pass: reason=%q", d.Reason)
	}
}

// PermissionRevoked removes a previously granted entry.
func TestPermissionTableRevoke(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("h", PermSpec{Mutate: []string{"tools.*"}})
	if !pt.CheckMutate("h", "tools.x").Allowed {
		t.Fatalf("setup: grant should allow")
	}
	pt.Revoke("h", PermSpec{Mutate: []string{"tools.*"}})
	if pt.CheckMutate("h", "tools.x").Allowed {
		t.Fatalf("after revoke, grant should not allow")
	}
}

// CheckMetaGrant requires the MetaGrant axis, not Mutate.
func TestPermissionTableMetaGrantSeparateFromMutate(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("h", PermSpec{Mutate: []string{"tools.*"}})
	d := pt.CheckMetaGrant("h", "tools.x")
	if d.Allowed {
		t.Fatalf("plain mutate grant must not confer meta.grant")
	}
	if d.Reason != "no meta-grant authority" {
		t.Fatalf("reason = %q, want no meta-grant authority", d.Reason)
	}
	pt.Grant("h", PermSpec{MetaGrant: []string{"tools.*"}})
	if !pt.CheckMetaGrant("h", "tools.x").Allowed {
		t.Fatalf("with meta.grant, must allow")
	}
}

// Spec idempotence: granting the same patterns twice does not duplicate
// them in the stored record. (Implementation detail, but it's the
// contract the loader leans on when replaying boot events.)
func TestPermissionTableGrantIdempotent(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("h", PermSpec{Mutate: []string{"tools.*"}})
	pt.Grant("h", PermSpec{Mutate: []string{"tools.*"}})
	got := pt.SpecFor("h")
	if len(got.Mutate) != 1 || got.Mutate[0] != "tools.*" {
		t.Fatalf("expected single tools.*, got %v", got.Mutate)
	}
}

// IsEmpty returns true for the zero value, false when any axis is set.
func TestPermSpecIsEmpty(t *testing.T) {
	if !(PermSpec{}).IsEmpty() {
		t.Fatalf("zero spec should be empty")
	}
	if (PermSpec{Mutate: []string{"x"}}).IsEmpty() {
		t.Fatalf("Mutate set should not be empty")
	}
	if (PermSpec{Read: []string{"x"}}).IsEmpty() {
		t.Fatalf("Read set should not be empty")
	}
	if (PermSpec{Forbidden: []string{"x"}}).IsEmpty() {
		t.Fatalf("Forbidden set should not be empty")
	}
	if (PermSpec{MetaGrant: []string{"x"}}).IsEmpty() {
		t.Fatalf("MetaGrant set should not be empty")
	}
}
