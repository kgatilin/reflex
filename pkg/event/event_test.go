package event

import (
	"encoding/json"
	"testing"
)

func TestStoreAppendAndSnapshot(t *testing.T) {
	s := NewStore()
	if s.Len() != 0 {
		t.Fatalf("expected empty store, got %d", s.Len())
	}
	s.Append(Event{ID: "1", Type: "A", RequestID: "r1"})
	s.Append(Event{ID: "2", Type: "B", RequestID: "r1"})
	s.Append(Event{ID: "3", Type: "A", RequestID: "r2"})

	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 events, got %d", len(snap))
	}
	// Snapshot must be a copy: mutating it must not affect store.
	snap[0].Type = "mutated"
	if s.Snapshot()[0].Type != "A" {
		t.Fatal("Snapshot did not return a defensive copy")
	}
}

func TestStoreSinceAndForRequest(t *testing.T) {
	s := NewStore()
	s.Append(Event{ID: "1", Type: "A", RequestID: "r1"})
	s.Append(Event{ID: "2", Type: "B", RequestID: "r2"})
	s.Append(Event{ID: "3", Type: "C", RequestID: "r1"})

	tail := s.Since(1)
	if len(tail) != 2 || tail[0].ID != "2" {
		t.Fatalf("Since(1) returned %+v", tail)
	}
	if got := s.Since(99); got != nil {
		t.Fatalf("Since past end should be nil, got %+v", got)
	}

	r1 := s.ForRequest("r1")
	if len(r1) != 2 {
		t.Fatalf("ForRequest(r1) expected 2, got %d", len(r1))
	}
}

func TestEventPayloadAs(t *testing.T) {
	type P struct {
		Tool string `json:"tool"`
		Args string `json:"args"`
	}
	raw, _ := json.Marshal(P{Tool: "calc", Args: "2+2"})
	e := Event{Payload: raw}
	var got P
	if err := e.PayloadAs(&got); err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if got.Tool != "calc" || got.Args != "2+2" {
		t.Fatalf("decoded payload = %+v", got)
	}

	empty := Event{}
	var sink P
	if err := empty.PayloadAs(&sink); err != nil {
		t.Fatalf("empty PayloadAs: %v", err)
	}
}
