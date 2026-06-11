package bus

import (
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/pkg/event"
)

// describedScopedSub is a fake subscriber with a custom declared scope.
type describedScopedSub struct {
	*recordingSub
	desc HandlerDescriptor
}

func (d *describedScopedSub) Descriptor() HandlerDescriptor { return d.desc }

func newScopedSub(name, scope, consumes, emit string) *describedScopedSub {
	d := HandlerDescriptor{Name: name, Consumes: consumes, Scope: scope}
	if emit != "" {
		d.Emits = append(d.Emits, EmittedDescriptor{Type: emit})
	}
	return &describedScopedSub{
		recordingSub: &recordingSub{name: name, matches: consumes, emit: []event.Event{{Type: emit}}},
		desc:         d,
	}
}

// TestRegisterCapturesDescriptorScope: a descriptor's Scope is recorded
// on the bus so subsequent permission checks resolve to that target.
func TestRegisterCapturesDescriptorScope(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("triage-tool", "triage.classify", "X", "Y"))
	if got := b.ScopeOf("triage-tool"); got != "triage.classify" {
		t.Fatalf("ScopeOf = %q, want triage.classify", got)
	}
}

// TestScopeOfDefaultsByName: handler without explicit scope gets
// "default.<name>".
func TestScopeOfDefaultsByName(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))
	if got := b.ScopeOf("h1"); got != "default.h1" {
		t.Fatalf("ScopeOf = %q, want default.h1", got)
	}
}

// HandlerRegistered payload includes the scope field.
func TestHandlerRegisteredPayloadHasScope(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("triage-tool", "triage.classify", "X", "Y"))
	snap := store.Snapshot()
	hr, ok := findMeta(snap, HandlerRegisteredType)
	if !ok {
		t.Fatal("no HandlerRegistered emitted")
	}
	var p struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(hr.Payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Scope != "triage.classify" {
		t.Fatalf("scope = %q, want triage.classify", p.Scope)
	}
}

// TestApplyBootGrantEmitsPermissionGranted: boot grants land on the
// event stream so the audit handler sees them.
func TestApplyBootGrantEmitsPermissionGranted(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.ApplyBootGrant("triage-tool", PermSpec{Mutate: []string{"triage.*"}})
	pg, ok := findMeta(store.Snapshot(), PermissionGrantedType)
	if !ok {
		t.Fatal("no PermissionGranted emitted")
	}
	if !pg.Terminal {
		t.Fatal("PermissionGranted must be terminal")
	}
	var p struct {
		Principal string   `json:"principal"`
		Mutate    []string `json:"mutate"`
		Granter   string   `json:"granter"`
	}
	_ = json.Unmarshal(pg.Payload, &p)
	if p.Principal != "triage-tool" || len(p.Mutate) != 1 || p.Mutate[0] != "triage.*" {
		t.Fatalf("payload = %+v", p)
	}
	if p.Granter != "boot" {
		t.Fatalf("granter = %q, want boot", p.Granter)
	}
}

// TestSubscribeAsAllowedWithGrant: a principal with mutate:[triage.*]
// can subscribe a handler in the triage.* scope.
func TestSubscribeAsAllowedWithGrant(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("triage-tool", "triage.classify", "RequestReceived", "Y"))
	b.ApplyBootGrant("stub", PermSpec{Mutate: []string{"triage.*"}})

	if err := b.SubscribeAs("stub", "triage-tool", "OtherEvent", 0); err != nil {
		t.Fatalf("SubscribeAs rejected: %v", err)
	}
	// SubscribeWithCheck emits a Subscribed event on accept; the
	// principal-attributed path should NOT emit PermissionDenied.
	if _, ok := findMeta(store.Snapshot(), PermissionDeniedType); ok {
		t.Fatal("unexpected PermissionDenied on allowed call")
	}
}

// TestSubscribeAsDeniedOutOfScope: a principal with no matching grant
// gets PermissionDenied{reason: "out_of_scope"} and the subscription
// is NOT recorded.
func TestSubscribeAsDeniedOutOfScope(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("triage-tool", "triage.classify", "RequestReceived", "Y"))
	// No grants for "rogue".
	err := b.SubscribeAs("rogue", "triage-tool", "OtherEvent", 0)
	if err == nil {
		t.Fatal("expected permission denied error")
	}
	pd, ok := findMeta(store.Snapshot(), PermissionDeniedType)
	if !ok {
		t.Fatal("no PermissionDenied emitted")
	}
	if !pd.Terminal {
		t.Fatal("PermissionDenied must be terminal")
	}
	var p struct {
		Principal   string `json:"principal"`
		Op          string `json:"op"`
		TargetScope string `json:"target_scope"`
		Reason      string `json:"reason"`
	}
	_ = json.Unmarshal(pd.Payload, &p)
	if p.Principal != "rogue" || p.Op != "subscribe" || p.Reason != "out_of_scope" {
		t.Fatalf("payload = %+v", p)
	}
	if p.TargetScope != "triage.classify" {
		t.Fatalf("target_scope = %q, want triage.classify", p.TargetScope)
	}
	// And the binding was NOT added (only the original Register subscription
	// remains).
	_, subs := b.LiveTable()
	for _, s := range subs {
		if s.Handler == "triage-tool" && s.EventType == "OtherEvent" {
			t.Fatal("forbidden subscription should not have been recorded")
		}
	}
}

// TestSubscribeAsDeniedForbidden: forbidden patterns take precedence
// over mutate.
func TestSubscribeAsDeniedForbidden(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("triage-tool", "triage.classify", "RequestReceived", "Y"))
	b.ApplyBootGrant("conflicted", PermSpec{
		Mutate:    []string{"*"},
		Forbidden: []string{"triage.*"},
	})

	err := b.SubscribeAs("conflicted", "triage-tool", "OtherEvent", 0)
	if err == nil {
		t.Fatal("expected forbidden denial")
	}
	pd, ok := findMeta(store.Snapshot(), PermissionDeniedType)
	if !ok {
		t.Fatal("no PermissionDenied emitted")
	}
	var p struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(pd.Payload, &p)
	if p.Reason != "forbidden" {
		t.Fatalf("reason = %q, want forbidden", p.Reason)
	}
}

// TestDefaultDenyOnReservedZones: no implicit "core.*" mutate exists for
// any principal; an explicit grant is required.
func TestDefaultDenyOnReservedZones(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("core-thing", "core.dispatcher", "X", "Y"))
	b.ApplyBootGrant("h", PermSpec{Mutate: []string{"*"}})

	err := b.SubscribeAs("h", "core-thing", "OtherEvent", 0)
	if err == nil {
		t.Fatal("default-deny on core.* should refuse even mutate:*")
	}
	pd, ok := findMeta(store.Snapshot(), PermissionDeniedType)
	if !ok {
		t.Fatal("no PermissionDenied emitted")
	}
	var p struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(pd.Payload, &p)
	if p.Reason != "forbidden" {
		t.Fatalf("reason = %q, want forbidden", p.Reason)
	}
}

// TestExplicitGrantOverridesReservedDeny: granting core.* explicitly
// lets the principal through.
func TestExplicitGrantOverridesReservedDeny(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("core-thing", "core.dispatcher", "X", "Y"))
	b.ApplyBootGrant("h", PermSpec{Mutate: []string{"core.*"}})

	if err := b.SubscribeAs("h", "core-thing", "OtherEvent", 0); err != nil {
		t.Fatalf("explicit core.* grant should pass: %v", err)
	}
}

// TestPublishPermissionGrantedRecursiveCheck: a principal without
// meta.grant cannot publish PermissionGranted.
func TestPublishPermissionGrantedRecursiveCheck(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.ApplyBootGrant("h", PermSpec{Mutate: []string{"triage.*"}})

	err := b.PublishPermissionGranted("h", "newcomer", PermSpec{Mutate: []string{"triage.*"}})
	if err == nil {
		t.Fatal("expected meta-grant denial")
	}
	pd, ok := findMeta(store.Snapshot(), PermissionDeniedType)
	if !ok {
		t.Fatal("no PermissionDenied emitted")
	}
	var p struct {
		Op     string `json:"op"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(pd.Payload, &p)
	if p.Op != OpMetaGrant {
		t.Fatalf("op = %q, want meta.grant", p.Op)
	}
	if p.Reason != "no meta-grant authority" {
		t.Fatalf("reason = %q", p.Reason)
	}
	// Table must be unchanged.
	if got := b.Permissions().SpecFor("newcomer"); len(got.Mutate) != 0 {
		t.Fatalf("table mutated despite denial: %+v", got)
	}
}

// TestPublishPermissionGrantedWithMetaGrant: principal with meta.grant
// covering the target scope succeeds.
func TestPublishPermissionGrantedWithMetaGrant(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.ApplyBootGrant("delegator", PermSpec{MetaGrant: []string{"triage.*"}})

	err := b.PublishPermissionGranted("delegator", "newcomer", PermSpec{Mutate: []string{"triage.*"}})
	if err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	// Two PermissionGranted events expected: the boot one + the new one.
	if got := countMeta(store.Snapshot(), PermissionGrantedType); got != 2 {
		t.Fatalf("PermissionGranted count = %d, want 2", got)
	}
	// Newcomer now has the grant.
	if !b.Permissions().CheckMutate("newcomer", "triage.x").Allowed {
		t.Fatal("newcomer should have the mutate right after grant")
	}
}

// TestPublishPermissionRevokedRemovesGrant: a revoke event removes a
// previously granted entry.
func TestPublishPermissionRevokedRemovesGrant(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.ApplyBootGrant("delegator", PermSpec{MetaGrant: []string{"triage.*"}})
	if err := b.PublishPermissionGranted("delegator", "h", PermSpec{Mutate: []string{"triage.*"}}); err != nil {
		t.Fatalf("setup grant: %v", err)
	}
	if err := b.PublishPermissionRevoked("delegator", "h", PermSpec{Mutate: []string{"triage.*"}}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if b.Permissions().CheckMutate("h", "triage.x").Allowed {
		t.Fatal("revoked grant should no longer allow")
	}
	if _, ok := findMeta(store.Snapshot(), PermissionRevokedType); !ok {
		t.Fatal("no PermissionRevoked emitted")
	}
}

// TestUnsubscribeAsAndDeregisterAsCheckPermissions: the other two
// mutation paths also enforce the layer.
func TestUnsubscribeAsAndDeregisterAsCheckPermissions(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(newScopedSub("triage-tool", "triage.classify", "X", "Y"))

	if err := b.UnsubscribeAs("rogue", "triage-tool", "X"); err == nil {
		t.Fatal("UnsubscribeAs should deny rogue")
	}
	if err := b.HandlerDeregisterAs("rogue", "triage-tool"); err == nil {
		t.Fatal("HandlerDeregisterAs should deny rogue")
	}
	// Two denies in the log.
	if got := countMeta(store.Snapshot(), PermissionDeniedType); got != 2 {
		t.Fatalf("PermissionDenied count = %d, want 2", got)
	}
}

// TestPermissionEventsExcludedFromEventDispatched: the three permission
// types join the meta-event class. Pure registration of a grant must
// not spawn EventDispatched.
func TestPermissionEventsExcludedFromEventDispatched(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.ApplyBootGrant("h", PermSpec{Mutate: []string{"triage.*"}})
	if got := countMeta(store.Snapshot(), EventDispatchedType); got != 0 {
		t.Fatalf("EventDispatched after grant = %d, want 0", got)
	}
}
