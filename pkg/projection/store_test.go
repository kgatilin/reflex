package projection

import (
	"encoding/json"
	"testing"
)

func TestStoreSetGetRoundTrip(t *testing.T) {
	s := NewStore()
	s.Set("req-1", "verdict", "STUCK")
	got, ok := s.Get("req-1", "verdict")
	if !ok {
		t.Fatalf("expected verdict to be set")
	}
	if got != "STUCK" {
		t.Fatalf("Get = %v, want STUCK", got)
	}
	if _, ok := s.Get("req-2", "verdict"); ok {
		t.Fatal("foreign request should be empty")
	}
}

func TestStoreOverwriteAndHas(t *testing.T) {
	s := NewStore()
	if s.Has("r", "k") {
		t.Fatal("empty store should not have key")
	}
	s.Set("r", "k", 1)
	s.Set("r", "k", 2)
	v, _ := s.Get("r", "k")
	if v != 2 {
		t.Fatalf("Get after overwrite = %v, want 2", v)
	}
	if !s.Has("r", "k") {
		t.Fatal("Has after Set should be true")
	}
}

func TestStoreForRequestAndSnapshot(t *testing.T) {
	s := NewStore()
	s.Set("r1", "a", "x")
	s.Set("r1", "b", "y")
	s.Set("r2", "a", "z")

	m := s.ForRequest("r1")
	if len(m) != 2 || m["a"] != "x" || m["b"] != "y" {
		t.Fatalf("ForRequest(r1) = %+v", m)
	}
	// Mutating the returned map must not affect the store.
	m["a"] = "tampered"
	if v, _ := s.Get("r1", "a"); v != "x" {
		t.Fatalf("returned ForRequest map was not a copy")
	}
	ids := s.RequestIDs()
	if len(ids) != 2 || ids[0] != "r1" || ids[1] != "r2" {
		t.Fatalf("RequestIDs = %v", ids)
	}
	snap := s.Snapshot()
	if len(snap) != 2 || snap["r2"]["a"] != "z" {
		t.Fatalf("Snapshot = %+v", snap)
	}
}

func TestStoreMarshalJSON(t *testing.T) {
	s := NewStore()
	s.Set("r1", "k", "v")
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if got["r1"]["k"] != "v" {
		t.Fatalf("round-trip = %+v", got)
	}
}

func TestStoreNilSafe(t *testing.T) {
	var s *Store
	s.Set("r", "k", 1) // must not panic
	if v, ok := s.Get("r", "k"); ok || v != nil {
		t.Fatalf("nil Get = (%v,%v), want (nil,false)", v, ok)
	}
	if s.Has("r", "k") {
		t.Fatal("nil Has should be false")
	}
}
