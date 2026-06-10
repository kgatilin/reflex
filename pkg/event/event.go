// Package event defines the canonical event type for reflex and an in-memory
// append-only event store. Everything that happens inside a reflex run is an
// event; subscribers react to events and produce new ones; state is derived
// by projecting the event log.
//
// The store is intentionally tiny: a slice behind a mutex. Reflex is a PoC
// for a single in-memory run, not a distributed log.
package event

import (
	"encoding/json"
	"sync"
	"time"
)

// Event is the canonical record on the reflex log.
//
// Every event carries a RequestID so that multi-request traces stay
// correlated; the dispatcher uses it to decide quiescence on a per-request
// basis. Type is the discriminator that handlers match on (e.g.
// "RequestReceived", "ToolCallProposed"). Payload is opaque JSON so handlers
// can attach arbitrary structured data without changing the core type.
//
// Terminal marks an event as an explicit leaf of the causal DAG: it is not
// expected to spawn any descendant events. Combined with CausedBy this lets
// reflex enforce a system-level invariant — every non-terminal event must
// have at least one child reaction; the post-drain orphan check (see
// CheckQuiescence) emits an EventOrphaned diagnostic otherwise.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	TS        time.Time       `json:"ts"`
	Source    string          `json:"source"`
	CausedBy  string          `json:"caused_by,omitempty"`
	Terminal  bool            `json:"terminal"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// New returns a non-terminal Event with the given type and payload. It is a
// convenience for handlers so they don't have to spell out the struct literal.
func New(typ string, payload json.RawMessage) Event {
	return Event{Type: typ, Payload: payload}
}

// NewTerminal returns a Terminal=true Event. Use it for events that close a
// causal chain (RequestHandled, RequestUnhandled, EventOrphaned, etc.).
func NewTerminal(typ string, payload json.RawMessage) Event {
	return Event{Type: typ, Payload: payload, Terminal: true}
}

// PayloadAs decodes the event payload into v. If the payload is empty it is
// treated as JSON null and v is left untouched.
func (e Event) PayloadAs(v any) error {
	if len(e.Payload) == 0 || string(e.Payload) == "null" {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// Store is the append-only log of events for one reflex run.
//
// Append is the only mutator. All readers see a consistent snapshot via
// Snapshot or Since, which copy the underlying slice so callers can iterate
// safely while new events arrive.
type Store struct {
	mu     sync.RWMutex
	events []Event
}

// NewStore returns an empty in-memory store.
func NewStore() *Store {
	return &Store{}
}

// Append records ev. The store does not assign IDs or timestamps — that is
// the dispatcher's job, so handlers see fully-formed events.
func (s *Store) Append(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// Snapshot returns a copy of all events appended so far.
func (s *Store) Snapshot() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// Since returns events at indices >= from, copied for safe iteration.
func (s *Store) Since(from int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if from < 0 {
		from = 0
	}
	if from >= len(s.events) {
		return nil
	}
	out := make([]Event, len(s.events)-from)
	copy(out, s.events[from:])
	return out
}

// Len returns the number of events in the store.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// ForRequest returns all events whose RequestID matches reqID.
func (s *Store) ForRequest(reqID string) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	for _, e := range s.events {
		if e.RequestID == reqID {
			out = append(out, e)
		}
	}
	return out
}
