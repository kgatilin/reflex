// Phase 4c — scope-based permission layer.
//
// This file holds the pure permission engine (scope matching, grant/revoke
// bookkeeping, decision function). The bus wires it into the control-plane
// mutation paths in bus.go; the loader publishes boot-time grants from YAML.
//
// Naming convention for scopes: dotted strings ("a.b.c"). A pattern ending
// in ".*" matches strings whose dotted prefix equals the part before ".*"
// — i.e. "a.b.*" matches "a.b.c" and "a.b.c.d" but NOT bare "a.b". A
// pattern of just "*" matches any non-empty dotted string (one or more
// components). Conservative matching: "*" never matches zero components.
//
// Defaults:
//   - A handler without a declared scope gets scope "default.*" and an
//     implicit grant mutate:[default.*] read:[*] so Phase 1–4b examples
//     continue to work without YAML changes.
//   - The reserved zones core.*, system.*, feedback.* are seeded with a
//     default-deny rule (forbidden grants to a synthetic "__default__"
//     principal). Specific handlers may be granted explicit access.

package bus

import (
	"sort"
	"strings"
	"sync"
)

// Permission operation tags used both in the runtime table and in the
// PermissionGranted/Revoked/Denied event payloads.
const (
	OpMutate    = "mutate"
	OpRead      = "read"
	OpForbidden = "forbidden"
	OpMetaGrant = "meta.grant"
)

// Phase 4c permission event types — terminal, sourced from the bus.
const (
	PermissionGrantedType = "PermissionGranted"
	PermissionRevokedType = "PermissionRevoked"
	PermissionDeniedType  = "PermissionDenied"
)

// DefaultScope is the implicit scope assigned to a handler whose YAML
// stanza (or SDK builder) does not declare one. Paired with an implicit
// grant of mutate:[default.*] read:[*] so default-scope handlers can
// continue to operate in the open default namespace.
const DefaultScope = "default"

// defaultPrincipal is the synthetic principal used to seed default-deny
// grants on reserved zones. The decision function never matches against
// it directly — its grants are folded into the "no explicit grant" case.
const defaultPrincipal = "__default__"

// ReservedZones returns the dotted prefixes the framework treats as
// default-deny for runtime mutation: core.*, system.*, feedback.*. A
// handler must hold an explicit mutate grant matching one of these
// prefixes to publish control-plane events targeting them.
func ReservedZones() []string {
	return []string{"core", "system", "feedback"}
}

// PermSpec is the declarative permission grant a handler carries. Empty
// slices are legal — meaning "no grants of this op". It maps to the
// inline YAML permissions: block and the SDK WithPermissions option.
type PermSpec struct {
	Mutate    []string
	Read      []string
	Forbidden []string
	MetaGrant []string
}

// IsEmpty reports whether spec carries no grants in any axis. Used by
// the loader to decide whether to emit PermissionGranted events.
func (s PermSpec) IsEmpty() bool {
	return len(s.Mutate) == 0 && len(s.Read) == 0 && len(s.Forbidden) == 0 && len(s.MetaGrant) == 0
}

// matchScope reports whether pattern matches target. Both are dotted
// strings; pattern may contain a trailing ".*" wildcard or be the bare
// "*". Matching is conservative: "*" only matches one or more
// components, never zero.
//
//	matchScope("triage.*", "triage.classify")           = true
//	matchScope("triage.*", "triage.classify.intent")    = true
//	matchScope("triage.*", "triage")                    = false
//	matchScope("triage.*", "analytics.foo")             = false
//	matchScope("*", "triage")                           = true
//	matchScope("*", "")                                 = false
//	matchScope("triage", "triage")                      = true (exact)
//	matchScope("triage", "triage.classify")             = false (exact requires equality)
func matchScope(pattern, target string) bool {
	if target == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if pattern == target {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-2]
		if prefix == "" {
			return target != ""
		}
		if target == prefix {
			return false
		}
		return strings.HasPrefix(target, prefix+".")
	}
	return false
}

// matchAny reports whether any pattern in patterns matches target.
func matchAny(patterns []string, target string) bool {
	for _, p := range patterns {
		if matchScope(p, target) {
			return true
		}
	}
	return false
}

// PermissionTable is the runtime fold of PermissionGranted /
// PermissionRevoked events. It is the source of truth the bus consults
// before letting a handler-issued control-plane event through.
//
// The table is in-memory only; on a fresh process boot the loader
// replays the PermissionGranted seed stream into a fresh table.
type PermissionTable struct {
	mu sync.RWMutex

	// grants[principal] -> per-op pattern lists. principal "__default__"
	// is used for reserved-zone default-deny seeds and is consulted in
	// the decision function only when no other principal-specific grant
	// matches.
	grants map[string]*PermSpec

	// reservedDeny[prefix] = true means runtime mutation of any target
	// under prefix.* is denied unless the principal has an explicit
	// matching mutate grant. Defaults: core, system, feedback.
	reservedDeny map[string]bool
}

// NewPermissionTable returns an empty table with the reserved zones
// (core/system/feedback) primed for default-deny. Boot-time grants land
// via Grant; runtime PermissionGranted events go through the same path.
func NewPermissionTable() *PermissionTable {
	t := &PermissionTable{
		grants:       map[string]*PermSpec{},
		reservedDeny: map[string]bool{},
	}
	for _, z := range ReservedZones() {
		t.reservedDeny[z] = true
	}
	return t
}

// Grant adds spec entries to principal's grant record. Idempotent for
// duplicate patterns. The bus calls this when applying a
// PermissionGranted event.
func (t *PermissionTable) Grant(principal string, spec PermSpec) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.grants[principal]
	if cur == nil {
		cur = &PermSpec{}
		t.grants[principal] = cur
	}
	cur.Mutate = mergePatterns(cur.Mutate, spec.Mutate)
	cur.Read = mergePatterns(cur.Read, spec.Read)
	cur.Forbidden = mergePatterns(cur.Forbidden, spec.Forbidden)
	cur.MetaGrant = mergePatterns(cur.MetaGrant, spec.MetaGrant)
}

// Revoke removes spec entries from principal's grant record. Idempotent
// for non-existent patterns. The bus calls this when applying a
// PermissionRevoked event.
func (t *PermissionTable) Revoke(principal string, spec PermSpec) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.grants[principal]
	if cur == nil {
		return
	}
	cur.Mutate = removePatterns(cur.Mutate, spec.Mutate)
	cur.Read = removePatterns(cur.Read, spec.Read)
	cur.Forbidden = removePatterns(cur.Forbidden, spec.Forbidden)
	cur.MetaGrant = removePatterns(cur.MetaGrant, spec.MetaGrant)
}

// SpecFor returns a copy of the current grant record for principal. The
// returned spec is safe to inspect; mutating it has no effect on the
// table.
func (t *PermissionTable) SpecFor(principal string) PermSpec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cur := t.grants[principal]
	if cur == nil {
		return PermSpec{}
	}
	return PermSpec{
		Mutate:    append([]string(nil), cur.Mutate...),
		Read:      append([]string(nil), cur.Read...),
		Forbidden: append([]string(nil), cur.Forbidden...),
		MetaGrant: append([]string(nil), cur.MetaGrant...),
	}
}

// Decision is the result of a permission check. Reason is set only on
// deny; it matches the strings the brief specifies for
// PermissionDenied.reason ("forbidden", "out_of_scope",
// "no meta-grant authority").
type Decision struct {
	Allowed bool
	Reason  string
}

// CheckMutate decides whether principal may publish a control-plane
// event targeting targetScope. forbidden wins over mutate; reserved
// zones require an explicit grant naming the reserved prefix (a bare
// "*" wildcard is not enough — the brief calls for default-deny that a
// careless mutate:[*] cannot bypass); out-of-scope is denied.
func (t *PermissionTable) CheckMutate(principal, targetScope string) Decision {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cur := t.grants[principal]
	// Explicit forbidden wins over everything else, including any
	// reserved-zone allowance.
	if cur != nil && matchAny(cur.Forbidden, targetScope) {
		return Decision{Allowed: false, Reason: "forbidden"}
	}
	// Reserved zones: default-deny unless the principal has an explicit
	// pattern naming the reserved prefix. matchExplicitReserved skips
	// bare "*" so a careless mutate:[*] cannot stumble into core/system/
	// feedback.
	for prefix, deny := range t.reservedDeny {
		if !deny {
			continue
		}
		if matchScope(prefix+".*", targetScope) || targetScope == prefix {
			if cur == nil || !matchExplicitReserved(cur.Mutate, prefix, targetScope) {
				return Decision{Allowed: false, Reason: "forbidden"}
			}
			// Explicit reserved grant authorises this target — skip
			// the generic mutate check below.
			return Decision{Allowed: true}
		}
	}
	if cur == nil || !matchAny(cur.Mutate, targetScope) {
		return Decision{Allowed: false, Reason: "out_of_scope"}
	}
	return Decision{Allowed: true}
}

// matchExplicitReserved reports whether any pattern in patterns
// (a) names a reserved-zone prefix (i.e. starts with prefix.) and
// (b) matches targetScope. A bare "*" is rejected because reserved
// zones require explicit acknowledgement.
func matchExplicitReserved(patterns []string, prefix, targetScope string) bool {
	for _, p := range patterns {
		if p == "*" {
			continue
		}
		// Pattern must be either exactly `prefix`, exactly `prefix.foo`,
		// or `prefix.*` — anything that BEGINS with the reserved prefix.
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		if p == prefix || strings.HasPrefix(p, prefix+".") {
			if matchScope(p, targetScope) {
				return true
			}
		}
	}
	return false
}

// CheckMetaGrant decides whether principal may publish a
// PermissionGranted event targeting targetScope. The check is identical
// in shape to CheckMutate but consults the MetaGrant axis instead.
func (t *PermissionTable) CheckMetaGrant(principal, targetScope string) Decision {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cur := t.grants[principal]
	if cur != nil && matchAny(cur.Forbidden, targetScope) {
		return Decision{Allowed: false, Reason: "forbidden"}
	}
	if cur == nil || !matchAny(cur.MetaGrant, targetScope) {
		return Decision{Allowed: false, Reason: "no meta-grant authority"}
	}
	return Decision{Allowed: true}
}

// Principals returns the sorted list of principals with at least one
// grant. Used by tests / debug dumps.
func (t *PermissionTable) Principals() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.grants))
	for k := range t.grants {
		if k == defaultPrincipal {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergePatterns returns a sorted, deduplicated union of a and b.
func mergePatterns(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			seen[x] = true
			a = append(a, x)
		}
	}
	sort.Strings(a)
	return a
}

// removePatterns returns a with every pattern present in b stripped out.
func removePatterns(a, b []string) []string {
	if len(b) == 0 || len(a) == 0 {
		return a
	}
	drop := map[string]bool{}
	for _, x := range b {
		drop[x] = true
	}
	out := a[:0]
	for _, x := range a {
		if drop[x] {
			continue
		}
		out = append(out, x)
	}
	return out
}

// scopeOfHandler returns the scope assigned to a handler by name, falling
// back to DefaultScope when no scope was declared. The bus stores per-
// handler scopes in its scopes map; the loader sets them at registration
// time.
//
// Note: scopes are the "what does this handler own" hint used when a
// target scope cannot be derived from the event payload (e.g. for
// HandlerRegistered the target IS the handler's declared scope).
func defaultScopeOf(name string) string {
	if name == "" {
		return DefaultScope + ".unknown"
	}
	return DefaultScope + "." + name
}
