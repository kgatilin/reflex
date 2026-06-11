package bus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/pkg/event"
)

// describedRecordingSub is a recordingSub that also exposes a Descriptor,
// so Register emits the control-plane events.
type describedRecordingSub struct {
	*recordingSub
	desc HandlerDescriptor
}

func (d *describedRecordingSub) Descriptor() HandlerDescriptor { return d.desc }

func makeDesc(name, consumes string, emits ...string) HandlerDescriptor {
	d := HandlerDescriptor{Name: name, Consumes: consumes}
	for _, e := range emits {
		d.Emits = append(d.Emits, EmittedDescriptor{Type: e})
	}
	return d
}

func describedSub(name, consumes, emit string) *describedRecordingSub {
	return &describedRecordingSub{
		recordingSub: &recordingSub{name: name, matches: consumes, emit: []event.Event{{Type: emit}}},
		desc:         makeDesc(name, consumes, emit),
	}
}

func TestRegisterEmitsHandlerRegisteredAndSubscribed(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))

	snap := store.Snapshot()
	if _, ok := findMeta(snap, HandlerRegisteredType); !ok {
		t.Fatalf("no HandlerRegistered emitted; types=%v", typesOf(snap))
	}
	if _, ok := findMeta(snap, SubscribedType); !ok {
		t.Fatalf("no Subscribed emitted; types=%v", typesOf(snap))
	}

	hr, _ := findMeta(snap, HandlerRegisteredType)
	if !hr.Terminal {
		t.Fatalf("HandlerRegistered must be terminal")
	}
	var hp struct {
		Name     string `json:"name"`
		Consumes string `json:"consumes"`
	}
	if err := json.Unmarshal(hr.Payload, &hp); err != nil {
		t.Fatalf("decode HandlerRegistered payload: %v", err)
	}
	if hp.Name != "h1" || hp.Consumes != "X" {
		t.Fatalf("HandlerRegistered payload = %+v", hp)
	}

	sb, _ := findMeta(snap, SubscribedType)
	if !sb.Terminal {
		t.Fatalf("Subscribed must be terminal")
	}
	var sp struct {
		HandlerName string `json:"handler_name"`
		EventType   string `json:"event_type"`
	}
	if err := json.Unmarshal(sb.Payload, &sp); err != nil {
		t.Fatalf("decode Subscribed payload: %v", err)
	}
	if sp.HandlerName != "h1" || sp.EventType != "X" {
		t.Fatalf("Subscribed payload = %+v", sp)
	}
}

func TestRegisterWithoutDescriptorEmitsNothing(t *testing.T) {
	// Test fakes that don't implement Described stay opaque — no
	// control-plane events fire for them, preserving back-compat.
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "ad-hoc", matches: "X"})

	snap := store.Snapshot()
	if _, ok := findMeta(snap, HandlerRegisteredType); ok {
		t.Fatalf("opaque subscriber should not emit HandlerRegistered")
	}
	if _, ok := findMeta(snap, SubscribedType); ok {
		t.Fatalf("opaque subscriber should not emit Subscribed")
	}
}

func TestUnsubscribeEmitsUnsubscribed(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))

	b.Unsubscribe("h1", "X")
	snap := store.Snapshot()
	un, ok := findMeta(snap, UnsubscribedType)
	if !ok {
		t.Fatalf("no Unsubscribed emitted")
	}
	if !un.Terminal {
		t.Fatalf("Unsubscribed must be terminal")
	}
	var p struct {
		HandlerName string `json:"handler_name"`
		EventType   string `json:"event_type"`
	}
	_ = json.Unmarshal(un.Payload, &p)
	if p.HandlerName != "h1" || p.EventType != "X" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestUnsubscribeUnknownIsNoOp(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Unsubscribe("does-not-exist", "X")
	if got := countMeta(store.Snapshot(), UnsubscribedType); got != 0 {
		t.Fatalf("Unsubscribed count = %d, want 0", got)
	}
}

func TestHandlerDeregisterEmitsAllRemovalEvents(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))
	// Reset by reading current count.
	beforeUn := countMeta(store.Snapshot(), UnsubscribedType)

	b.HandlerDeregister("h1")
	snap := store.Snapshot()
	if got := countMeta(snap, UnsubscribedType) - beforeUn; got != 1 {
		t.Fatalf("Unsubscribed count delta = %d, want 1", got)
	}
	if got := countMeta(snap, HandlerDeregisteredType); got != 1 {
		t.Fatalf("HandlerDeregistered count = %d, want 1", got)
	}
	// Subscriber is also removed from dispatch.
	if got := len(b.Subscribers()); got != 0 {
		t.Fatalf("after deregister subscribers = %d, want 0", got)
	}
}

func TestSubscribeWithCheckAcceptsAcyclic(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))

	// New binding h1 → Z (no other handler emits Z, can't form a cycle).
	if err := b.SubscribeWithCheck("h1", "Z", 0); err != nil {
		t.Fatalf("SubscribeWithCheck rejected an acyclic binding: %v", err)
	}
	snap := store.Snapshot()
	// 2 Subscribed events expected — the initial one from Register + the
	// runtime one from SubscribeWithCheck.
	if got := countMeta(snap, SubscribedType); got != 2 {
		t.Fatalf("Subscribed count = %d, want 2", got)
	}
}

func TestSubscribeWithCheckRejectsUnknownHandler(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	err := b.SubscribeWithCheck("nobody", "X", 0)
	if err == nil {
		t.Fatal("expected error for unknown handler")
	}
	if _, ok := findMeta(store.Snapshot(), SubscriptionRejectedType); !ok {
		t.Fatal("expected SubscriptionRejected event")
	}
}

func TestSubscribeWithCheckRejectsUncappedCycle(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	// A consumes X, emits Y; B consumes Y, emits X. The first Register
	// of A and B already creates the table; the cycle is captured the
	// moment B subscribes to Y because A emits Y → B → A.
	b.Register(describedSub("A", "X", "Y"))
	// Register B but without subscribing it yet — to get an unrelated
	// initial state we register it with a non-cyclic consume, then add
	// the cycling subscription via SubscribeWithCheck so the rejection
	// path fires there.
	b.Register(describedSub("B", "Y", "X"))
	// Both A and B are in the table now. The original Register already
	// fanned them out — verify a redundant SubscribeWithCheck of an
	// existing-but-cycle-forming edge is rejected.
	err := b.SubscribeWithCheck("A", "X", 0)
	if err == nil {
		t.Fatal("expected uncapped cycle rejection")
	}
	sj, ok := findMeta(store.Snapshot(), SubscriptionRejectedType)
	if !ok {
		t.Fatalf("expected SubscriptionRejected event; types=%v", typesOf(store.Snapshot()))
	}
	var p struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(sj.Payload, &p)
	if p.Reason == "" {
		t.Fatalf("SubscriptionRejected reason must be set")
	}
}

func TestSubscribeWithCheckAcceptsCappedCycle(t *testing.T) {
	store := event.NewStore()
	b := New(store, WithLoopCaps(map[string]int{"B": 3}))
	b.Register(describedSub("A", "X", "Y"))
	b.Register(describedSub("B", "Y", "X"))
	// Even though A→B→A is a cycle, B has a loop cap → table is acceptable.
	if scc, ok := b.CheckLiveTableCycles(); !ok {
		t.Fatalf("capped cycle should be acceptable; scc=%v", scc)
	}
}

func TestCheckLiveTableCyclesIgnoresTerminalEmissions(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	// A emits Y terminal → no real cycle even though B subscribes to Y
	// and emits X back.
	a := describedSub("A", "X", "Y")
	a.desc.Emits[0].Terminal = true
	b.Register(a)
	b.Register(describedSub("B", "Y", "X"))
	if scc, ok := b.CheckLiveTableCycles(); !ok {
		t.Fatalf("terminal emission should not close a cycle; scc=%v", scc)
	}
}

func TestLiveTableSnapshotReturnsCurrentState(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))
	descs, subs := b.LiveTable()
	if _, ok := descs["h1"]; !ok {
		t.Fatalf("descriptor for h1 missing")
	}
	if len(subs) != 1 || subs[0].Handler != "h1" || subs[0].EventType != "X" {
		t.Fatalf("subscriptions = %+v", subs)
	}
}

func TestControlPlaneEventsExcludedFromEventDispatched(t *testing.T) {
	// Phase 1.6 invariant: meta events do not spawn EventDispatched. The
	// control-plane events join the same class — otherwise registering N
	// handlers would emit N HandlerRegistered + N EventDispatched even
	// when no domain event has fired.
	store := event.NewStore()
	b := New(store)
	b.Register(describedSub("h1", "X", "Y"))
	if got := countMeta(store.Snapshot(), EventDispatchedType); got != 0 {
		t.Fatalf("EventDispatched count after pure registration = %d, want 0", got)
	}
	_ = context.Background()
}
