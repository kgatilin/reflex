// Projection store: a per-request key/value map handlers can read and write.
//
// SessionProjection is a fold of the event log; it cannot be written to from
// inside a handler (the log is reality). Some patterns nonetheless want a
// place to stash a structured intermediate result that downstream handlers
// can pick up by key — a triage verdict, an extracted entity, a parsed plan.
//
// The Store provides that channel. It lives in the runtime, is in-memory,
// per `reflex run`, and is keyed by request_id then by user-chosen key.
// Handlers write into it via the SDK-style helpers on bus.Subscriber's
// context (see pkg/bus); CLI wait-predicates can block until a key appears.
//
// The store is not a substitute for events. It is a side-channel for
// projection material — explicitly so a handler can express "I have decided
// X" without re-emitting an event every time a downstream reader needs to
// know. Anything that should affect causal structure stays an event.
package projection

import (
	"encoding/json"
	"sort"
	"sync"
)

// Store is a per-request key/value projection map. Methods are safe for
// concurrent use.
type Store struct {
	mu   sync.RWMutex
	data map[string]map[string]any
}

// NewStore returns an empty projection store.
func NewStore() *Store {
	return &Store{data: map[string]map[string]any{}}
}

// Set writes key=value into the projection for requestID. value is stored
// as-is; callers may pass any JSON-serialisable type. Overwriting a key is
// allowed — the projection is a mutable view, not an event log.
func (s *Store) Set(requestID, key string, value any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data[requestID]
	if !ok {
		m = map[string]any{}
		s.data[requestID] = m
	}
	m[key] = value
}

// Get reads key for requestID. The second return is false when the key (or
// the request) is absent.
func (s *Store) Get(requestID, key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.data[requestID]
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

// Has reports whether requestID has a value for key.
func (s *Store) Has(requestID, key string) bool {
	_, ok := s.Get(requestID, key)
	return ok
}

// ForRequest returns a copy of the projection map for requestID. nil when
// the request has no entries yet. The copy is shallow — value types pass
// through unchanged.
func (s *Store) ForRequest(requestID string) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.data[requestID]
	if !ok {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// RequestIDs returns the set of request IDs that have at least one entry,
// in sorted order for deterministic trace output.
func (s *Store) RequestIDs() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Snapshot returns a deep-enough copy of the entire projection state,
// shaped as request_id → key → value. Used to embed the projection in trace
// output at the end of a run.
func (s *Store) Snapshot() map[string]map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]any, len(s.data))
	for rid, m := range s.data {
		inner := make(map[string]any, len(m))
		for k, v := range m {
			inner[k] = v
		}
		out[rid] = inner
	}
	return out
}

// MarshalJSON implements json.Marshaler so the projection can be embedded
// in a trace file as a single JSON object.
func (s *Store) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	return json.Marshal(s.Snapshot())
}
