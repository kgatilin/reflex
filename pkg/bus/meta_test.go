package bus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kgatilin/reflex/pkg/event"
)

// findMeta returns the first event whose type matches typ; ok=false if absent.
func findMeta(snap []event.Event, typ string) (event.Event, bool) {
	for _, e := range snap {
		if e.Type == typ {
			return e, true
		}
	}
	return event.Event{}, false
}

// countMeta returns how many events with the given type are in snap.
func countMeta(snap []event.Event, typ string) int {
	n := 0
	for _, e := range snap {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestBusEmitsEventDispatchedAfterFanout(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "A", matches: "Trigger", emit: []event.Event{{Type: "X"}}})
	b.Register(&recordingSub{name: "B", matches: "Trigger", emit: []event.Event{{Type: "Y"}}})

	if err := b.Run(context.Background(), event.Event{Type: "Trigger", RequestID: "r"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snap := store.Snapshot()
	ed, ok := findMeta(snap, EventDispatchedType)
	if !ok {
		t.Fatalf("no EventDispatched emitted; types=%v", typesOf(snap))
	}
	if !ed.Terminal {
		t.Fatalf("EventDispatched must be terminal")
	}
	var p struct {
		EventType       string `json:"event_type"`
		SubscriberCount int    `json:"subscriber_count"`
	}
	if err := json.Unmarshal(ed.Payload, &p); err != nil {
		t.Fatalf("decode EventDispatched payload: %v", err)
	}
	// The first EventDispatched corresponds to the Trigger event with two
	// matching subscribers (A + B).
	if p.EventType != "Trigger" {
		t.Fatalf("first EventDispatched.event_type = %q, want Trigger", p.EventType)
	}
	if p.SubscriberCount != 2 {
		t.Fatalf("subscriber_count = %d, want 2", p.SubscriberCount)
	}
}

func TestBusEmitsDrainQuiescedPerRequest(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "A", matches: "X", emit: []event.Event{{Type: "Y"}}})

	if err := b.Run(context.Background(), event.Event{Type: "X", RequestID: "req-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snap := store.Snapshot()
	dq, ok := findMeta(snap, DrainQuiescedType)
	if !ok {
		t.Fatalf("no DrainQuiesced emitted")
	}
	if !dq.Terminal {
		t.Fatalf("DrainQuiesced must be terminal")
	}
	if dq.RequestID != "req-1" {
		t.Fatalf("DrainQuiesced.request_id = %q, want req-1", dq.RequestID)
	}
	if got := countMeta(snap, DrainQuiescedType); got != 1 {
		t.Fatalf("DrainQuiesced count = %d, want 1", got)
	}
}

func TestBusEmitsHandlerFailedOnReactError(t *testing.T) {
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "explodes", matches: "X", err: errors.New("kaboom")})

	err := b.Run(context.Background(), event.Event{Type: "X", RequestID: "r"})
	if err == nil {
		t.Fatal("expected dispatcher error")
	}
	snap := store.Snapshot()
	hf, ok := findMeta(snap, HandlerFailedType)
	if !ok {
		t.Fatalf("no HandlerFailed emitted; types=%v", typesOf(snap))
	}
	if !hf.Terminal {
		t.Fatalf("HandlerFailed must be terminal")
	}
	var p struct {
		HandlerName string `json:"handler_name"`
		EventType   string `json:"event_type"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(hf.Payload, &p); err != nil {
		t.Fatalf("decode HandlerFailed payload: %v", err)
	}
	if p.HandlerName != "explodes" {
		t.Fatalf("handler_name = %q, want explodes", p.HandlerName)
	}
	if p.EventType != "X" {
		t.Fatalf("event_type = %q, want X", p.EventType)
	}
	if p.Error != "kaboom" {
		t.Fatalf("error = %q, want kaboom", p.Error)
	}
}

func TestBusDoesNotEmitMetaForMeta(t *testing.T) {
	// EventDispatched-of-EventDispatched would be infinite. Verify we
	// emit exactly one EventDispatched per non-meta routed event.
	store := event.NewStore()
	b := New(store)
	b.Register(&recordingSub{name: "A", matches: "X", emit: []event.Event{{Type: "Y"}}})

	if err := b.Run(context.Background(), event.Event{Type: "X", RequestID: "r"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two non-meta events were routed: X (seed) and Y (emitted). One
	// EventDispatched per — no recursion.
	if got := countMeta(store.Snapshot(), EventDispatchedType); got != 2 {
		t.Fatalf("EventDispatched count = %d, want 2", got)
	}
}

func typesOf(snap []event.Event) []string {
	out := make([]string, len(snap))
	for i, e := range snap {
		out[i] = e.Type
	}
	return out
}
