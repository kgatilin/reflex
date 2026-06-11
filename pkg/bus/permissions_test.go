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
		{"triage", "triage", true},
		{"triage.classify", "triage.classify", true},
		// Exact requires equality — no prefix promotion.
		{"triage", "triage.classify", false},
		// Trailing .* matches one or more additional components.
		{"triage.*", "triage.classify", true},
		{"triage.*", "triage.classify.intent", true},
		// Conservative: "triage.*" does NOT match bare "triage".
		{"triage.*", "triage", false},
		// Mismatched prefix.
		{"triage.*", "analytics.foo", false},
		// Bare "*" matches any non-empty target.
		{"*", "triage", true},
		{"*", "analytics.metrics", true},
		{"*", "", false},
		// Empty target never matches.
		{"triage.*", "", false},
		// Dotted prefix sentinel — pattern containing "." but not ending
		// in ".*" must be an exact match.
		{"triage.classify", "triage.classify.intent", false},
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
	d := pt.CheckMutate("rogue", "triage.classify")
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
	pt.Grant("triage-tool", PermSpec{Mutate: []string{"triage.*"}})
	d := pt.CheckMutate("triage-tool", "triage.classify")
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
	if !pt.CheckMutate("broad", "triage.x").Allowed {
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
	pt.Grant("h", PermSpec{Mutate: []string{"triage.*"}})
	if !pt.CheckMutate("h", "triage.x").Allowed {
		t.Fatalf("setup: grant should allow")
	}
	pt.Revoke("h", PermSpec{Mutate: []string{"triage.*"}})
	if pt.CheckMutate("h", "triage.x").Allowed {
		t.Fatalf("after revoke, grant should not allow")
	}
}

// CheckMetaGrant requires the MetaGrant axis, not Mutate.
func TestPermissionTableMetaGrantSeparateFromMutate(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("h", PermSpec{Mutate: []string{"triage.*"}})
	d := pt.CheckMetaGrant("h", "triage.x")
	if d.Allowed {
		t.Fatalf("plain mutate grant must not confer meta.grant")
	}
	if d.Reason != "no meta-grant authority" {
		t.Fatalf("reason = %q, want no meta-grant authority", d.Reason)
	}
	pt.Grant("h", PermSpec{MetaGrant: []string{"triage.*"}})
	if !pt.CheckMetaGrant("h", "triage.x").Allowed {
		t.Fatalf("with meta.grant, must allow")
	}
}

// Spec idempotence: granting the same patterns twice does not duplicate
// them in the stored record. (Implementation detail, but it's the
// contract the loader leans on when replaying boot events.)
func TestPermissionTableGrantIdempotent(t *testing.T) {
	pt := NewPermissionTable()
	pt.Grant("h", PermSpec{Mutate: []string{"triage.*"}})
	pt.Grant("h", PermSpec{Mutate: []string{"triage.*"}})
	got := pt.SpecFor("h")
	if len(got.Mutate) != 1 || got.Mutate[0] != "triage.*" {
		t.Fatalf("expected single triage.*, got %v", got.Mutate)
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
