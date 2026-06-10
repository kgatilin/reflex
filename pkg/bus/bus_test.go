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
	if store.Len() != 2 {
		t.Fatalf("expected 2 events, got %d", store.Len())
	}
	if store.Snapshot()[1].RequestID != "r1" {
		t.Fatal("emitted event did not inherit request_id")
	}
	if store.Snapshot()[1].CausedBy != store.Snapshot()[0].ID {
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
	got := []string{}
	for _, e := range store.Snapshot() {
		got = append(got, e.Type)
	}
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
