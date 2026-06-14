package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// catalogTopology is a small, connected, catalog-bearing topology: an ingress
// resolver, a worker, and a sink, with EventKind decls registering every kind
// the nodes emit/consume. It exercises the static catalog checks (unknown-kind,
// dead-subscription) without dragging in the full example topology.
func catalogTopology() []Decl {
	objSchema := json.RawMessage(`{"type":"object"}`)
	return []Decl{
		EventKind{Kind: "request.received", Schema: objSchema},
		EventKind{Kind: "work.done", Schema: objSchema},

		Subscriber{Name: "resolver", On: []string{"app.ingress.*"}, In: "global", Emits: []string{"request.received"}},
		Subscriber{Name: "worker", On: []string{"request.received"}, In: "global", Emits: []string{"work.done"}},
		Subscriber{Name: "sink", On: []string{"work.done"}, In: "global"},
	}
}

func TestCatalog_RegistrationMakesAKindValidAndAdvertisable(t *testing.T) {
	// A topology declaring EventKinds for every emitted/consumed kind validates
	// clean, and the folded catalog returns the right kind→schema (doc 26 §4a).
	decls := catalogTopology()
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Connected {
		t.Fatalf("expected Connected==true for a fully-registered topology; gaps: unknown=%v dead-subs=%v dead=%v unreach=%v",
			rep.UnknownKinds, rep.DeadSubscriptions, rep.DeadEnds, rep.UnreachableNodes)
	}

	cat := foldCatalog(decls, nil)
	if cat.empty() {
		t.Fatal("expected a non-empty catalog from the EventKind decls")
	}
	for _, k := range []string{"request.received", "work.done"} {
		if !cat.has(k) {
			t.Fatalf("expected catalog to know %q; kinds=%v", k, cat.kinds())
		}
		s, ok := cat.schemaOf(k)
		if !ok || string(s) != `{"type":"object"}` {
			t.Fatalf("expected schema {\"type\":\"object\"} for %q; got ok=%v schema=%s", k, ok, s)
		}
	}
	// The primordial seed is always known and cannot be the only entry that makes
	// the catalog "present".
	if !cat.has(seedKind) {
		t.Fatalf("expected the primordial seed kind %q to always be known", seedKind)
	}
}

func TestCatalog_UnknownKindIsRejected(t *testing.T) {
	// A node emits a kind not registered, in a topology that DOES declare a
	// catalog: unknown-kind fires, Connected==false, the kind is reported with a
	// register-it suggestion (doc 26 §4a / 27 §5).
	decls := catalogTopology()
	// Mutate the worker to emit an unregistered kind alongside work.done.
	for i := range decls {
		if n, ok := decls[i].(Subscriber); ok && n.Name == "worker" {
			n.Emits = []string{"work.done", "work.mystery"}
			decls[i] = n
		}
	}
	// work.mystery has no consumer either, but the catalog check is what we assert
	// — add a consumer so the only gap is unknown-kind.
	decls = append(decls, Subscriber{Name: "mystery-sink", On: []string{"work.mystery"}, In: "global"})

	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !contains(rep.UnknownKinds, "work.mystery") {
		t.Fatalf("expected work.mystery in unknown-kinds; got %v", rep.UnknownKinds)
	}
	if rep.Connected {
		t.Fatal("expected Connected==false with an unknown emitted kind")
	}
	if !anyContains(rep.Suggestions, "work.mystery") || !anyContains(rep.Suggestions, "register") {
		t.Fatalf("expected a register-it suggestion for work.mystery; got %v", rep.Suggestions)
	}

	// Registering it closes the gap.
	fixed := append(decls, EventKind{Kind: "work.mystery", Schema: json.RawMessage(`{"type":"object"}`)})
	rep2, err := Validate(fixed...)
	if err != nil {
		t.Fatalf("Validate (fixed): %v", err)
	}
	if len(rep2.UnknownKinds) != 0 {
		t.Fatalf("expected no unknown kinds once registered; got %v", rep2.UnknownKinds)
	}
	if !rep2.Connected {
		t.Fatalf("expected Connected==true once work.mystery is registered; gaps: unknown=%v dead-subs=%v dead=%v",
			rep2.UnknownKinds, rep2.DeadSubscriptions, rep2.DeadEnds)
	}
}

func TestCatalog_DeadSubscriptionIsReported(t *testing.T) {
	// An On pattern matching no catalog kind (catalog non-empty) is a dead
	// subscription — it can never fire (doc 26 §4a / 27 §5).
	decls := catalogTopology()
	// The sink subscribes to a pattern no catalog kind matches.
	for i := range decls {
		if n, ok := decls[i].(Subscriber); ok && n.Name == "sink" {
			n.On = []string{"never.matches.anything"}
			decls[i] = n
		}
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !contains(rep.DeadSubscriptions, "never.matches.anything") {
		t.Fatalf("expected never.matches.anything in dead-subscriptions; got %v", rep.DeadSubscriptions)
	}
	if rep.Connected {
		t.Fatal("expected Connected==false with a dead subscription")
	}
	if !anyContains(rep.Suggestions, "never.matches.anything") || !anyContains(rep.Suggestions, "never fire") {
		t.Fatalf("expected a dead-subscription suggestion; got %v", rep.Suggestions)
	}
}

func TestCatalog_OptInDormancyLeavesCatalogLessTopologyUnaffected(t *testing.T) {
	// The critical non-breaking rule (doc 26 §4a): a catalog-less topology — no
	// EventKind decls, no event.registered facts — must validate exactly as
	// before. The example topology declares no catalog, so the two new checks are
	// dormant and report nothing.
	decls := exampleTopology()
	for _, d := range decls {
		if _, ok := d.(EventKind); ok {
			t.Fatal("precondition: example topology must declare no EventKind")
		}
	}
	rep, err := Validate(decls...)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Connected {
		t.Fatalf("expected the catalog-less example topology to stay Connected; gaps: unknown=%v dead-subs=%v",
			rep.UnknownKinds, rep.DeadSubscriptions)
	}
	if len(rep.UnknownKinds) != 0 || len(rep.DeadSubscriptions) != 0 {
		t.Fatalf("expected dormant catalog checks (no catalog declared); got unknown=%v dead-subs=%v",
			rep.UnknownKinds, rep.DeadSubscriptions)
	}

	cat := foldCatalog(decls, nil)
	if !cat.empty() {
		t.Fatal("expected an empty catalog (only the seed) for the catalog-less topology")
	}
}

func TestCatalog_DynamicRegistrationGrowsTheFold(t *testing.T) {
	// An event.registered fact appended at runtime makes a previously-unknown
	// kind known to the catalog fold (doc 26 §4a self-hosting). The fold reads the
	// log, so the dynamic registration is reflected without any decl.
	log := []Event{
		{
			Subject: "sys.event.registered",
			Payload: RegisterPayload("plugin.tool.call", json.RawMessage(`{"type":"object","required":["path"]}`)),
		},
	}
	// Before folding the log, only the seed is known.
	declOnly := foldCatalog(nil, nil)
	if declOnly.has("plugin.tool.call") {
		t.Fatal("plugin.tool.call should be unknown before the registration fact is folded")
	}
	// Folding the log reflects the dynamic registration.
	cat := foldCatalog(nil, log)
	if !cat.has("plugin.tool.call") {
		t.Fatalf("expected plugin.tool.call known after registration; kinds=%v", cat.kinds())
	}
	if cat.empty() {
		t.Fatal("expected a non-empty catalog after a runtime registration")
	}
	s, ok := cat.schemaOf("plugin.tool.call")
	if !ok || !strings.Contains(string(s), `"required":["path"]`) {
		t.Fatalf("expected the registered schema folded in; got ok=%v schema=%s", ok, s)
	}
}

// conformanceTopology drives one node that emits a payload we control, with a
// catalog schema (an EventKind decl) that the payload either satisfies or
// violates. The node fires once on an ingress event. Like the drain tests, it
// installs decls directly (the test seam) and exercises Drain — the runtime
// payload-conformance path, not Validate — so the resolver's On is the ingress
// event's kind tail (matching the dispatch convention of drain_test.go).
func conformanceTopology(t *testing.T, emit Emit, schema json.RawMessage) *Engine {
	t.Helper()
	e := New()
	e.install(
		EventKind{Kind: "echo.done", Schema: schema},
		Subscriber{
			Name:  "resolver",
			On:    []string{"surface.in"}, // kind tail of app.ingress.surface.in
			In:    "global",
			Emits: []string{"echo.done"},
			Body: ReactionFunc(func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
				return []Emit{emit}, nil
			}),
		},
		// A sink so echo.done has a consumer (matches the runtime fanout shape).
		Subscriber{Name: "sink", On: []string{"echo.done"}, In: "global"},
	)
	if _, err := e.Append(context.Background(), "app.ingress.surface.in", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append ingress: %v", err)
	}
	if err := e.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	return e
}

func TestCatalog_NonConformingPayloadBecomesFailed(t *testing.T) {
	// An emit whose payload violates its kind's schema becomes {node}.failed, the
	// drain continues, and no panic occurs (doc 26 §4a runtime half / G3). The
	// schema requires a string field "name"; the payload omits it.
	schema := json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`)
	bad := Emit{Kind: "echo.done", Payload: json.RawMessage(`{"other":1}`)}
	e := conformanceTopology(t, bad, schema)

	var sawFailed, sawEchoDone bool
	for ev := range e.Events() {
		_, _, kind := splitSubject(ev.Subject)
		switch kind {
		case "resolver.failed":
			sawFailed = true
			if !strings.Contains(string(ev.Payload), "echo.done") {
				t.Fatalf("expected the failed payload to name the offending kind; got %s", ev.Payload)
			}
		case "echo.done":
			sawEchoDone = true
		}
	}
	if !sawFailed {
		t.Fatal("expected a resolver.failed event for the non-conforming payload")
	}
	if sawEchoDone {
		t.Fatal("expected the non-conforming echo.done to NOT be appended (it became .failed instead)")
	}
}

func TestCatalog_ConformingPayloadIsEmittedNormally(t *testing.T) {
	// The same topology with a conforming payload emits echo.done normally — the
	// conformance check is transparent when the payload is valid.
	schema := json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`)
	good := Emit{Kind: "echo.done", Payload: json.RawMessage(`{"name":"ok"}`)}
	e := conformanceTopology(t, good, schema)

	var sawFailed, sawEchoDone bool
	for ev := range e.Events() {
		_, _, kind := splitSubject(ev.Subject)
		switch kind {
		case "resolver.failed":
			sawFailed = true
		case "echo.done":
			sawEchoDone = true
		}
	}
	if sawFailed {
		t.Fatal("did not expect resolver.failed for a conforming payload")
	}
	if !sawEchoDone {
		t.Fatal("expected echo.done to be emitted for a conforming payload")
	}
}

func TestConforms_SubsetBehaviour(t *testing.T) {
	// Direct unit coverage of the documented JSON-Schema subset.
	cases := []struct {
		name    string
		schema  string
		payload string
		wantErr bool
	}{
		{"nil schema conforms", ``, `{"anything":true}`, false},
		{"required present", `{"required":["a"]}`, `{"a":1}`, false},
		{"required missing", `{"required":["a"]}`, `{"b":1}`, true},
		{"type string ok", `{"properties":{"a":{"type":"string"}}}`, `{"a":"x"}`, false},
		{"type string violated", `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`, true},
		{"type number ok", `{"properties":{"a":{"type":"number"}}}`, `{"a":1.5}`, false},
		{"type integer ok", `{"properties":{"a":{"type":"integer"}}}`, `{"a":3}`, false},
		{"type integer violated by float", `{"properties":{"a":{"type":"integer"}}}`, `{"a":3.5}`, true},
		{"type boolean ok", `{"properties":{"a":{"type":"boolean"}}}`, `{"a":true}`, false},
		{"absent property not enforced", `{"properties":{"a":{"type":"string"}}}`, `{}`, false},
		{"non-object payload against object schema", `{"type":"object"}`, `"a string"`, true},
		{"non-object top type is permissive", `{"type":"string"}`, `"a string"`, false},
		{"unparseable schema is permissive", `{not json`, `{}`, false},
		{"unknown declared type permissive", `{"properties":{"a":{"type":"weird"}}}`, `{"a":1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := conforms(json.RawMessage(tc.payload), json.RawMessage(tc.schema))
			if tc.wantErr && err == nil {
				t.Fatalf("expected a conformance error; got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected conformance; got %v", err)
			}
		})
	}
}
