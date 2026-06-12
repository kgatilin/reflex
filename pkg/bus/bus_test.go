package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/event"
)

type recordingSub struct {
	name    string
	matches string
	emit    []event.Event
	calls   int
	err     error
}

func (r *recordingSub) Name() string             { return r.name }
func (r *recordingSub) Match(e event.Event) bool { return e.Type == r.matches }
func (r *recordingSub) React(_ context.Context, _ event.Event, _ []event.Event) ([]event.Event, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.emit, nil
}

// nonMeta returns ev types from snap that are NOT bus meta-events. Used by
// dispatcher tests that pre-date Phase 1.6 — those tests assert the user
// chain, not the meta routing layer the bus emits around it.
func nonMeta(snap []event.Event) []string {
	out := []string{}
	for _, e := range snap {
		switch e.Type {
		case EventDispatchedType, DrainQuiescedType, HandlerFailedType, "scope.quiesced":
			continue
		}
		out = append(out, e.Type)
	}
	return out
}

func TestDispatcherFiresMatchingSubscriber(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	sub := &recordingSub{name: "A", matches: "RequestReceived", emit: []event.Event{{Type: "AssistantMessageProposed"}}}
	b.Register(sub)

	err := b.Run(context.Background(), event.Event{Type: "RequestReceived", RequestID: "r1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sub.calls != 1 {
		t.Fatalf("expected 1 call, got %d", sub.calls)
	}
	user := nonMeta(store.Snapshot())
	if len(user) != 2 {
		t.Fatalf("expected 2 user events, got %d (%v)", len(user), user)
	}
	// Locate the user emission and assert its lineage to the seed.
	var seed, userEv event.Event
	for _, e := range store.Snapshot() {
		switch e.Type {
		case "RequestReceived":
			seed = e
		case "AssistantMessageProposed":
			userEv = e
		}
	}
	if userEv.RequestID != "r1" {
		t.Fatal("emitted event did not inherit request_id")
	}
	if userEv.CausedBy != seed.ID {
		t.Fatal("emitted event did not record caused_by")
	}
}

func TestDispatcherChainsEvents(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "A", matches: "X", emit: []event.Event{{Type: "Y"}}})
	b.Register(&recordingSub{name: "B", matches: "Y", emit: []event.Event{{Type: "Z"}}})

	if err := b.Run(context.Background(), event.Event{Type: "X", RequestID: "r"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := nonMeta(store.Snapshot())
	want := []string{"X", "Y", "Z"}
	if len(got) != len(want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestDispatcherStopsOnSubscriberError(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "boom", matches: "X", err: errors.New("kaboom")})
	err := b.Run(context.Background(), event.Event{Type: "X", RequestID: "r"})
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("expected kaboom error, got %v", err)
	}
}

func TestDispatcherMaxStepsAborts(t *testing.T) {
	store := event.NewStore()
	b := New(store, WithMaxSteps(5))
	// Self-feeding loop: X -> X.
	b.Register(&recordingSub{name: "loop", matches: "X", emit: []event.Event{{Type: "X"}}})
	err := b.Run(context.Background(), event.Event{Type: "X", RequestID: "r"})
	if err == nil || !strings.Contains(err.Error(), "max steps") {
		t.Fatalf("expected max steps error, got %v", err)
	}
}

func TestQuiescenceOneSubscriberEmitsOneWithNoSubscribers(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	sub := &recordingSub{
		name:    "A",
		matches: "SeedEvent",
		emit: []event.Event{
			{Type: "ChildEvent"},
		},
	}
	b.Register(sub)

	err := b.Run(context.Background(), event.Event{Type: "SeedEvent", RequestID: "r-test1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Check store for scope.quiesced event
	var foundQuiesced bool
	for _, ev := range store.Snapshot() {
		if ev.Type == "scope.quiesced" {
			if ev.RequestID != "r-test1" {
				t.Errorf("expected request_id r-test1, got %q", ev.RequestID)
			}
			foundQuiesced = true
		}
	}
	if !foundQuiesced {
		t.Error("expected to find scope.quiesced event in store")
	}
}

func TestQuiescenceTwoSubscribersEmittingTerminal(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	sub1 := &recordingSub{
		name:    "A",
		matches: "SeedEvent",
		emit: []event.Event{
			{Type: "Term1", Terminal: true},
		},
	}
	sub2 := &recordingSub{
		name:    "B",
		matches: "SeedEvent",
		emit: []event.Event{
			{Type: "Term2", Terminal: true},
		},
	}
	b.Register(sub1)
	b.Register(sub2)

	err := b.Run(context.Background(), event.Event{Type: "SeedEvent", RequestID: "r-test2"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify scope.quiesced is present
	var quiescedEvs []event.Event
	for _, ev := range store.Snapshot() {
		if ev.Type == "scope.quiesced" {
			quiescedEvs = append(quiescedEvs, ev)
		}
	}
	if len(quiescedEvs) != 1 {
		t.Fatalf("expected exactly 1 scope.quiesced event, got %d", len(quiescedEvs))
	}
	if quiescedEvs[0].RequestID != "r-test2" {
		t.Errorf("expected request_id r-test2, got %q", quiescedEvs[0].RequestID)
	}

	// Also assert that scope.quiesced is emitted AFTER both Term1 and Term2 are in the store
	var term1Idx, term2Idx, quiescedIdx int = -1, -1, -1
	for i, ev := range store.Snapshot() {
		if ev.Type == "Term1" {
			term1Idx = i
		}
		if ev.Type == "Term2" {
			term2Idx = i
		}
		if ev.Type == "scope.quiesced" {
			quiescedIdx = i
		}
	}
	if term1Idx == -1 || term2Idx == -1 || quiescedIdx == -1 {
		t.Fatalf("missing events: Term1=%d, Term2=%d, quiesced=%d", term1Idx, term2Idx, quiescedIdx)
	}
	if quiescedIdx < term1Idx || quiescedIdx < term2Idx {
		t.Errorf("expected scope.quiesced to be appended after both Term1 and Term2, got indices: Term1=%d, Term2=%d, quiesced=%d", term1Idx, term2Idx, quiescedIdx)
	}
}

func TestQuiescenceNonTerminalSeedZeroSubscribers(t *testing.T) {
	store := event.NewStore()
	b := New(store)

	// Zero subscribers registered

	err := b.Run(context.Background(), event.Event{Type: "SeedEvent", RequestID: "r-test3"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var quiescedEvs []event.Event
	for _, ev := range store.Snapshot() {
		if ev.Type == "scope.quiesced" {
			quiescedEvs = append(quiescedEvs, ev)
		}
	}
	if len(quiescedEvs) != 1 {
		t.Fatalf("expected exactly 1 scope.quiesced event for orphan seed, got %d", len(quiescedEvs))
	}
	if quiescedEvs[0].RequestID != "r-test3" {
		t.Errorf("expected request_id r-test3, got %q", quiescedEvs[0].RequestID)
	}
}
