package changeset

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
)

// reqPayload mirrors the engine's changeset request payload so the test can read
// the ops the bridge assembled from the draft. engine.Op is exported.
type reqPayload struct {
	Ops       []engine.Op `json:"ops"`
	Principal string      `json:"principal"`
}

// draftViews is a fake engine.Views whose `draft` view is a fixed []Event — the
// accumulated add-* pieces. Everything else is the null object.
type draftViews struct{ draft []engine.Event }

func (v draftViews) Value(name string) any {
	if name == "topology.draft" {
		return v.draft
	}
	return nil
}
func (draftViews) KV(string) engine.KV                          { return nil }
func (v draftViews) Log(name string) []engine.Event             { return v.draft }
func (draftViews) Schema(string) (json.RawMessage, bool)        { return nil, false }

func react(t *testing.T, cfg Config, kind string, payload string, views engine.Views) []engine.Emit {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	r, err := Factory(engine.Subscriber{Name: "bridge", BodyConfig: raw})
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	ev := engine.Event{Subject: kind, Payload: json.RawMessage(payload)}
	emits, err := r.React(context.Background(), ev, views)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return emits
}

// addEvent builds a draft entry: an add-* event of the given kind + payload.
func addEvent(kind string, payload string) engine.Event {
	return engine.Event{Subject: kind, Payload: json.RawMessage(payload)}
}

// TestBridge_AddIsSilent proves an add-* piece emits NOTHING — it just accumulates
// on the log (the draft projection folds it). The brain re-drives once per TURN via
// the scope-closure barrier, not per add, so a multi-call turn does not branch.
func TestBridge_AddIsSilent(t *testing.T) {
	for _, kind := range []string{KindScopeAdd, KindEventAdd, KindSubscriberAdd, KindProjectionAdd} {
		emits := react(t, Config{}, kind, `{"name":"x"}`, draftViews{})
		if len(emits) != 0 {
			t.Errorf("%s emitted %+v, want nothing (adds are silent)", kind, emits)
		}
	}
}

// TestBridge_BuildDedupesByName proves a draft with repeated/refined adds of the
// same name collapses to one decl (last-writer-wins) — so a branched/iterating
// brain still assembles a clean topology.
func TestBridge_BuildDedupesByName(t *testing.T) {
	draft := []engine.Event{
		addEvent(KindScopeAdd, `{"name":"request","root":"old"}`),
		addEvent(KindScopeAdd, `{"name":"request","root":"new","detached":true}`), // refine same name
		addEvent(KindSubscriberAdd, `{"name":"worker","in":"request"}`),
		addEvent(KindSubscriberAdd, `{"name":"worker","in":"request"}`), // duplicate
	}
	decls, err := declsFromDraft(draft)
	if err != nil {
		t.Fatalf("declsFromDraft: %v", err)
	}
	var scopes, subs int
	for _, d := range decls {
		switch v := d.(type) {
		case engine.Scope:
			scopes++
			if v.Root != "new" || !v.Detached {
				t.Errorf("scope not last-writer-wins: %+v", v)
			}
		case engine.Subscriber:
			subs++
		}
	}
	if scopes != 1 || subs != 1 {
		t.Fatalf("deduped decls: scopes=%d subs=%d, want 1 and 1", scopes, subs)
	}
}

// TestBridge_BuildAssemblesDraft is the core: topology.build folds the accumulated
// add-* pieces into ONE changeset request whose ops carry the right decls.
func TestBridge_BuildAssemblesDraft(t *testing.T) {
	views := draftViews{draft: []engine.Event{
		addEvent(KindScopeAdd, `{"name":"request","root":"request.received","detached":true,"budget":{"llm.message":8}}`),
		addEvent(KindEventAdd, `{"kind":"request.terminal","terminal":true}`),
		addEvent(KindSubscriberAdd, `{"name":"worker","on":["request.received"],"in":"request","emits":["llm.message"],"body":{"kind":"llm","config":{"model":"vertex:gemini-3.5-flash"}}}`),
		addEvent(KindProjectionAdd, `{"name":"worker.history","on":["request.received"],"in":"request","type":"llm.history"}`),
	}}
	emits := react(t, Config{}, KindBuild, `{}`, views)
	if len(emits) != 1 || emits[0].Kind != engine.KindChangesetRequested {
		t.Fatalf("emits = %+v, want one changeset request", emits)
	}
	var rp reqPayload
	if err := json.Unmarshal(emits[0].Payload, &rp); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if rp.Principal != defaultPrinc {
		t.Errorf("principal = %q, want %q", rp.Principal, defaultPrinc)
	}
	// 1 scope + 1 event + 1 subscriber + 1 projection = 4 add-ops.
	if len(rp.Ops) != 4 {
		t.Fatalf("ops = %d, want 4", len(rp.Ops))
	}
	kinds := map[string]int{}
	for _, op := range rp.Ops {
		if op.Verb != "add" {
			t.Errorf("op %q verb = %q, want add", op.Name, op.Verb)
		}
		kinds[op.Kind]++
	}
	for _, want := range []string{"scope", "event", "subscriber", "projection"} {
		if kinds[want] != 1 {
			t.Errorf("op kind %q count = %d, want 1 (got %v)", want, kinds[want], kinds)
		}
	}
}

// TestBridge_BuildDecodesScopeDetachedAndBody proves the structured fields survive
// into the decls: the detached flag and the subscriber's nested body descriptor.
func TestBridge_BuildDecodesScopeDetachedAndBody(t *testing.T) {
	draft := []engine.Event{
		addEvent(KindScopeAdd, `{"name":"request","root":"request.received","detached":true}`),
		addEvent(KindSubscriberAdd, `{"name":"worker","in":"request","body":{"kind":"llm","config":{"model":"m"}}}`),
	}
	decls, err := declsFromDraft(draft)
	if err != nil {
		t.Fatalf("declsFromDraft: %v", err)
	}
	var sawDetached, sawBody bool
	for _, d := range decls {
		switch v := d.(type) {
		case engine.Scope:
			if v.Name == "request" && v.Detached {
				sawDetached = true
			}
		case engine.Subscriber:
			if v.Name == "worker" && v.BodyKind == "llm" && len(v.BodyConfig) > 0 {
				sawBody = true
			}
		}
	}
	if !sawDetached {
		t.Error("scope detached flag was lost")
	}
	if !sawBody {
		t.Error("subscriber body descriptor was lost")
	}
}

// TestBridge_DispatchAutoBuilds proves dispatch (task.new) AUTO-BUILDS the draft
// AND emits the worker entry kind (request.received) carrying the task payload, as
// siblings — so the brain never has to call build: composing + dispatching launches.
func TestBridge_DispatchAutoBuilds(t *testing.T) {
	views := draftViews{draft: []engine.Event{
		addEvent(KindScopeAdd, `{"name":"request","root":"request.received","detached":true}`),
		addEvent(KindSubscriberAdd, `{"name":"worker","on":["request.received"],"in":"request"}`),
	}}
	emits := react(t, Config{}, defaultDispatch, `{"task":"fix the bug"}`, views)
	if len(emits) != 2 {
		t.Fatalf("emits = %+v, want 2 (build + entry)", emits)
	}
	if emits[0].Kind != engine.KindChangesetRequested {
		t.Errorf("emit[0] = %q, want the build changeset request", emits[0].Kind)
	}
	if emits[1].Kind != defaultDispatchEmit {
		t.Fatalf("emit[1] = %q, want the worker entry %q", emits[1].Kind, defaultDispatchEmit)
	}
	// The entry carries the task payload through to the worker.
	var p map[string]string
	_ = json.Unmarshal(emits[1].Payload, &p)
	if p["task"] != "fix the bug" {
		t.Errorf("entry payload = %v, want it to carry the task text", p)
	}
	// And the build it emits is the assembled draft (2 ops).
	var rp reqPayload
	_ = json.Unmarshal(emits[0].Payload, &rp)
	if len(rp.Ops) != 2 {
		t.Errorf("build ops = %d, want 2", len(rp.Ops))
	}
}

// TestBridge_BuildEmptyDraftFails proves committing an empty draft is rejected with
// guidance (not a vacuous apply).
func TestBridge_BuildEmptyDraftFails(t *testing.T) {
	emits := react(t, Config{}, KindBuild, `{}`, draftViews{})
	if len(emits) != 1 || emits[0].Kind != defaultFail {
		t.Fatalf("emits = %+v, want one fail emit", emits)
	}
	var body map[string]string
	_ = json.Unmarshal(emits[0].Payload, &body)
	if !contains(body["error"], "draft is empty") {
		t.Errorf("error = %q, want it to say the draft is empty", body["error"])
	}
}

// TestBridge_BuildMalformedPieceFails proves a corrupt draft entry aborts the build
// naming the offending kind.
func TestBridge_BuildMalformedPieceFails(t *testing.T) {
	views := draftViews{draft: []engine.Event{
		addEvent(KindScopeAdd, `{"name":"ok","root":"x"}`),
		addEvent(KindSubscriberAdd, `{"on": "not-an-array"}`),
	}}
	emits := react(t, Config{}, KindBuild, `{}`, views)
	if len(emits) != 1 || emits[0].Kind != defaultFail {
		t.Fatalf("emits = %+v, want one fail emit", emits)
	}
	var body map[string]string
	_ = json.Unmarshal(emits[0].Payload, &body)
	if !contains(body["error"], KindSubscriberAdd) {
		t.Errorf("error = %q, want it to name %q", body["error"], KindSubscriberAdd)
	}
}

// TestCatalog_AdvertisesStructuredSchemas proves the bridge self-registers each
// control kind in its On WITH a structured (typed) schema — the whole point: the
// model calls a typed function, not a free-text document field.
func TestCatalog_AdvertisesStructuredSchemas(t *testing.T) {
	s := engine.Subscriber{
		Name: "bridge",
		On:   []string{KindScopeAdd, KindSubscriberAdd, KindBuild},
	}
	kinds := Catalog(s)
	byKind := map[string]engine.EventKind{}
	for _, k := range kinds {
		byKind[k.Kind] = k
	}
	for _, want := range []string{KindScopeAdd, KindSubscriberAdd} {
		ek, ok := byKind[want]
		if !ok || len(ek.Schema) == 0 {
			t.Errorf("%q not advertised with a schema (got %+v)", want, ek)
			continue
		}
		if !contains(string(ek.Schema), `"properties"`) || !contains(string(ek.Schema), `"name"`) {
			t.Errorf("%q schema is not a structured object: %s", want, ek.Schema)
		}
	}
	// The fail feedback kind is registered too.
	if _, ok := byKind[defaultFail]; !ok {
		t.Errorf("fail kind %q not registered", defaultFail)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}
