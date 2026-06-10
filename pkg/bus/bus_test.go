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
		case EventDispatchedType, DrainQuiescedType, HandlerFailedType:
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
