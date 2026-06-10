package main

import (
	"testing"

	"github.com/kgatilin/reflex/internal/runtime"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

func TestCheckWaitPredicateDrain(t *testing.T) {
	res := &runtime.Result{
		RequestID: "r",
		Events: []event.Event{
			{Type: "RequestReceived", RequestID: "r"},
			{Type: projection.TypeDrainQuiesced, RequestID: "r", Terminal: true},
		},
	}
	if ok, why := checkWaitPredicate("drain", res); !ok {
		t.Fatalf("drain predicate should have resolved: %s", why)
	}

	noDrain := &runtime.Result{
		RequestID: "r",
		Events:    []event.Event{{Type: "RequestReceived", RequestID: "r"}},
	}
	if ok, _ := checkWaitPredicate("drain", noDrain); ok {
		t.Fatal("drain predicate must not pass without DrainQuiesced")
	}
}

func TestCheckWaitPredicateRequestIDTerminal(t *testing.T) {
	res := &runtime.Result{
		RequestID: "r",
		Events: []event.Event{
			{Type: "RequestReceived", RequestID: "r"},
			{Type: "RequestHandled", RequestID: "r", Terminal: true},
		},
	}
	if ok, why := checkWaitPredicate("request_id_terminal", res); !ok {
		t.Fatalf("request_id_terminal should resolve: %s", why)
	}

	// Meta-events alone should NOT satisfy the predicate.
	metaOnly := &runtime.Result{
		RequestID: "r",
		Events: []event.Event{
			{Type: "RequestReceived", RequestID: "r"},
			{Type: projection.TypeEventDispatched, RequestID: "r", Terminal: true},
			{Type: projection.TypeDrainQuiesced, RequestID: "r", Terminal: true},
		},
	}
	if ok, _ := checkWaitPredicate("request_id_terminal", metaOnly); ok {
		t.Fatal("request_id_terminal must ignore meta-events as terminals")
	}
}

func TestCheckWaitPredicateProjectionHas(t *testing.T) {
	p := projection.NewStore()
	p.Set("r", "triage.verdict", map[string]any{"classification": "STUCK"})
	res := &runtime.Result{RequestID: "r", Projection: p}

	if ok, why := checkWaitPredicate("projection.has=triage.verdict", res); !ok {
		t.Fatalf("projection.has should resolve: %s", why)
	}
	if ok, _ := checkWaitPredicate("projection.has=missing", res); ok {
		t.Fatal("projection.has must not resolve for missing keys")
	}
	if ok, _ := checkWaitPredicate("projection.has=", res); ok {
		t.Fatal("projection.has= with empty key must not resolve")
	}
}

func TestCheckWaitPredicateUnknownReturnsFalse(t *testing.T) {
	res := &runtime.Result{RequestID: "r"}
	ok, why := checkWaitPredicate("never_seen", res)
	if ok {
		t.Fatal("unknown predicate must not resolve")
	}
	if why == "" {
		t.Fatal("unknown predicate should explain why")
	}
}
