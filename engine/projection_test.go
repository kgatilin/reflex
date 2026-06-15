package engine

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
)

// observed captures what a reaction saw through its Views at React time, keyed
// by the reaction's trigger span so a test can assert per-firing what was in
// the causal past. It is test scaffolding, not engine state.
type observed struct {
	kv  map[string]map[string]string // triggerSpan → (key → value-as-string)
	log map[string][]string          // triggerSpan → matched kinds in log order
}

func newObserved() *observed {
	return &observed{
		kv:  map[string]map[string]string{},
		log: map[string][]string{},
	}
}

// readerNode builds a node that, on each firing, records the named kv and log
// views (evaluated at its trigger) into obs, then emits emitKindOut so the
// chain can continue if needed.
func readerNode(name string, on []string, in string, reads []string, kvName, logName, emitKindOut string, obs *observed) Subscriber {
	return Subscriber{
		Name:  name,
		On:    on,
		In:    in,
		Reads: reads,
		Emits: []string{emitKindOut},
		Body: ReactionFunc(func(_ context.Context, ev Event, v Views) ([]Emit, error) {
			span := ev.Trace.SpanID
			if kvName != "" {
				kv := v.KV(kvName)
				m := map[string]string{}
				for _, k := range kv.Keys() {
					val, _ := kv.Get(k)
					m[k] = jsonToString(val)
				}
				obs.kv[span] = m
			}
			if logName != "" {
				var kinds []string
				for _, le := range v.Log(logName) {
					_, _, kind := splitSubject(le.Subject)
					kinds = append(kinds, kind)
				}
				obs.log[span] = kinds
			}
			if emitKindOut == "" {
				return nil, nil
			}
			return []Emit{{Kind: emitKindOut, Payload: json.RawMessage(`{}`)}}, nil
		}),
	}
}

// TestProjection_KVAndLogViewsReproduceTheFold is Stage 2c acceptance test 1: a
// reaction reads a declared kv projection and a log projection that reproduce
// the expected fold from the trigger's causal PAST (doc 24 §6). The chain writes
// three state.updated facts, then a reader fires and asserts the kv folded them
// (last-writer-wins on a repeated path) and the log listed them in log order.
func TestProjection_KVAndLogViewsReproduceTheFold(t *testing.T) {
	ctx := context.Background()
	obs := newObserved()

	// writer emits, on request.received: status=gathering, goal="ship it",
	// status=planning (overwrites status — last writer wins), then a probe event
	// that the reader fires on. The kv keys by the path tail after
	// "state.updated." (default Key) and stores the payload (default Value).
	writer := Subscriber{
		Name:  "writer",
		On:    []string{"request.received"},
		In:    "request",
		Emits: []string{"state.updated.status", "state.updated.goal", "probe.go"},
		Body: ReactionFunc(func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
			return []Emit{
				{Kind: "state.updated.status", Payload: json.RawMessage(`{"v":"gathering"}`)},
				{Kind: "state.updated.goal", Payload: json.RawMessage(`{"v":"ship it"}`)},
				{Kind: "state.updated.status", Payload: json.RawMessage(`{"v":"planning"}`)},
				{Kind: "probe.go", Payload: json.RawMessage(`{}`)},
			}, nil
		}),
	}
	reader := readerNode(
		"reader",
		[]string{"probe.go"},
		"request",
		[]string{"task_state", "task_log"},
		"task_state", "task_log", "",
		obs,
	)

	decls := []Decl{
		Scope{Name: "request", Root: "request.received"},
		Subscriber{Name: "resolver", On: []string{"test.msg"}, Emits: []string{"request.received"}, Body: emitKind("request.received")},
		writer,
		reader,
		// kv view over the per-scope state writes, keyed by path (default Key),
		// valued by the whole payload (default Value).
		Projection{Name: "task_state", On: []string{"state.updated.>"}, In: HorizonRequest, Type: TypeKV},
		// log view: the matched state writes in log order.
		Projection{Name: "task_log", On: []string{"state.updated.>"}, In: HorizonRequest, Type: TypeLog},
	}

	e := New()
	e.install(decls...)
	if _, err := e.Append(ctx, "test.msg", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Exactly one reader firing.
	if len(obs.kv) != 1 {
		t.Fatalf("expected exactly one reader firing, got %d (kv=%v)", len(obs.kv), obs.kv)
	}
	var kv map[string]string
	var logKinds []string
	for span, m := range obs.kv {
		kv = m
		logKinds = obs.log[span]
	}

	// kv folded the two kinds (default Key = the event's kind tail); the status
	// key holds "planning" — the last writer over the two status writes wins, in
	// log order (doc 24 §6).
	if got, want := kv["state.updated.status"], `{"v":"planning"}`; got != want {
		t.Fatalf("task_state[state.updated.status] = %q, want %q (last-writer-wins) — full kv %v", got, want, kv)
	}
	if got, want := kv["state.updated.goal"], `{"v":"ship it"}`; got != want {
		t.Fatalf("task_state[state.updated.goal] = %q, want %q — full kv %v", got, want, kv)
	}
	if len(kv) != 2 {
		t.Fatalf("task_state has %d keys, want 2 (status,goal) — %v", len(kv), kv)
	}

	// log view lists all three state writes in log order (status,goal,status) —
	// the log shape does NOT dedupe.
	wantLog := []string{"state.updated.status", "state.updated.goal", "state.updated.status"}
	if len(logKinds) != len(wantLog) {
		t.Fatalf("task_log = %v, want %v", logKinds, wantLog)
	}
	for i := range wantLog {
		if logKinds[i] != wantLog[i] {
			t.Fatalf("task_log[%d] = %q, want %q (full %v)", i, logKinds[i], wantLog[i], logKinds)
		}
	}
}

// TestProjection_ReadAtTriggerIsolationAcrossParallelRequestCones is Stage 2c
// acceptance test 2 (the key correctness test): two request instances run in one
// Drain; each reaction's request-horizon view sees ONLY its own cone's facts,
// never the sibling's. The two external events carry distinct payloads so the
// resolver writes a distinct goal in each cone; the reader in cone A must read
// cone A's goal and not cone B's, and vice versa — read-at-trigger isolation is
// the disjoint backward walk (doc 26 §2a).
func TestProjection_ReadAtTriggerIsolationAcrossParallelRequestCones(t *testing.T) {
	ctx := context.Background()
	obs := newObserved()

	// resolver, on the external event, copies the "tag" into state.updated.goal
	// for its own request cone, then emits probe.go for the reader.
	resolver := Subscriber{
		Name:  "resolver",
		On:    []string{"test.msg"},
		Emits: []string{"request.received"},
		Body:  emitKind("request.received"),
	}
	tagger := Subscriber{
		Name:  "tagger",
		On:    []string{"request.received"},
		In:    "request",
		Emits: []string{"state.updated.goal", "probe.go"},
		Body: ReactionFunc(func(_ context.Context, ev Event, _ Views) ([]Emit, error) {
			// The external event tag rode the request.received payload (the resolver's
			// emitKind emits `{}`, so we instead read the request_id to make the
			// per-cone value distinct). Use the request id as the goal value.
			val, _ := json.Marshal(map[string]string{"req": ev.Trace.RequestID})
			return []Emit{
				{Kind: "state.updated.goal", Payload: val},
				{Kind: "probe.go", Payload: json.RawMessage(`{}`)},
			}, nil
		}),
	}
	reader := readerNode(
		"reader",
		[]string{"probe.go"},
		"request",
		[]string{"task_state"},
		"task_state", "", "",
		obs,
	)

	decls := []Decl{
		Scope{Name: "request", Root: "request.received"},
		resolver,
		tagger,
		reader,
		Projection{Name: "task_state", On: []string{"state.updated.>"}, In: HorizonRequest, Type: TypeKV},
	}

	e := New()
	e.install(decls...)
	// Two external events ⇒ two parallel request cones in one Drain.
	if _, err := e.Append(ctx, "test.msg", json.RawMessage(`{"tag":"A"}`)); err != nil {
		t.Fatalf("Append A: %v", err)
	}
	if _, err := e.Append(ctx, "test.msg", json.RawMessage(`{"tag":"B"}`)); err != nil {
		t.Fatalf("Append B: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Two reader firings, one per cone.
	if len(obs.kv) != 2 {
		t.Fatalf("expected two reader firings (one per cone), got %d: %v", len(obs.kv), obs.kv)
	}

	// Each firing's view holds exactly ONE goal, and it is its OWN cone's goal:
	// the goal value encodes the request id, which is the cone's root span. The
	// view's "goal" must reference the same request id the reader fired under.
	for span, m := range obs.kv {
		if len(m) != 1 {
			t.Fatalf("firing %s saw %d keys, want exactly 1 (its own cone's goal) — %v", span, len(m), m)
		}
		goalRaw, ok := m["state.updated.goal"]
		if !ok {
			t.Fatalf("firing %s view has no goal — %v", span, m)
		}
		var g struct {
			Req string `json:"req"`
		}
		if err := json.Unmarshal([]byte(goalRaw), &g); err != nil {
			t.Fatalf("firing %s goal not decodable: %v", span, err)
		}
		// The reader's own request id: walk caused_by back to the request root.
		readerReq := requestIDOf(e, span)
		if g.Req != readerReq {
			t.Fatalf("firing %s (request %q) read goal of request %q — sibling cone leaked in", span, readerReq, g.Req)
		}
	}

	// Sanity: the two cones carried DISTINCT goals (no accidental aliasing).
	seen := map[string]struct{}{}
	for _, m := range obs.kv {
		seen[m["state.updated.goal"]] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("the two cones must carry distinct goals, got %v", seen)
	}
}

// requestIDOf returns the RequestID stamped on the event with the given span.
func requestIDOf(e *Engine, span string) string {
	for ev := range e.Events() {
		if ev.Trace.SpanID == span {
			return ev.Trace.RequestID
		}
	}
	return ""
}

// TestProjection_PromoteViaClosure is Stage 2c acceptance test 3: a
// request-scoped node writes state.updated.found; on scope.request.closed a
// GLOBAL-scoped consumer reads the snapshot from the closure payload and writes
// state.updated.project_context into the GLOBAL state. The global state reflects
// it; the request states do not leak into each other (doc 26 §2a — promotion is
// ONLY through closure, writes are local).
func TestProjection_PromoteViaClosure(t *testing.T) {
	ctx := context.Background()

	// finder (in request): writes state.updated.found into its own cone.
	finder := Subscriber{
		Name:  "finder",
		On:    []string{"request.received"},
		In:    "request",
		Emits: []string{"state.updated.found"},
		Body: ReactionFunc(func(_ context.Context, ev Event, _ Views) ([]Emit, error) {
			val, _ := json.Marshal(map[string]string{"path": "main.go", "req": ev.Trace.RequestID})
			return []Emit{{Kind: "state.updated.found", Payload: val}}, nil
		}),
	}
	// promoter (global): consumes scope.request.closed, reads the closing cone's
	// state snapshot from the payload, and promotes "found" into the GLOBAL state
	// as state.updated.project_context. This is the only request→global path.
	promoter := Subscriber{
		Name:  "promoter",
		On:    []string{"scope.request.closed"},
		In:    "global",
		Emits: []string{"state.updated.project_context"},
		Body: ReactionFunc(func(_ context.Context, ev Event, _ Views) ([]Emit, error) {
			var p closedPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, err
			}
			found, ok := p.State["found"]
			if !ok {
				return nil, nil // nothing to promote
			}
			return []Emit{{Kind: "state.updated.project_context", Payload: found}}, nil
		}),
	}

	decls := []Decl{
		Scope{Name: "request", Root: "request.received"},
		Subscriber{Name: "resolver", On: []string{"test.msg"}, Emits: []string{"request.received"}, Body: emitKind("request.received")},
		finder,
		promoter,
		// global state view: folds the global-horizon state writes.
		Projection{Name: "global_state", On: []string{"state.updated.>"}, In: HorizonGlobal, Type: TypeKV},
	}

	e := New()
	e.install(decls...)
	if _, err := e.Append(ctx, "test.msg", json.RawMessage(`{"tag":"A"}`)); err != nil {
		t.Fatalf("Append A: %v", err)
	}
	if _, err := e.Append(ctx, "test.msg", json.RawMessage(`{"tag":"B"}`)); err != nil {
		t.Fatalf("Append B: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Two request cones closed, each carrying its own found in the snapshot.
	closed := scopeFactsOf(e, "scope.request.closed")
	if len(closed) != 2 {
		t.Fatalf("expected 2 request closures, got %d", len(closed))
	}
	for _, p := range closed {
		foundRaw, ok := p.State["found"]
		if !ok {
			t.Fatalf("closure %s carries no found in snapshot — %v", p.Instance, p.State)
		}
		var f struct {
			Req string `json:"req"`
		}
		_ = json.Unmarshal([]byte(foundRaw), &f)
		// Each closure's snapshot holds ONLY its own cone's write (writes are
		// local; no sibling leak): the found's req == this instance's root span.
		if f.Req != p.Instance {
			t.Fatalf("closure %s snapshot carries found of request %q — sibling state leaked across cones", p.Instance, f.Req)
		}
		// And exactly one key (found): the request cone wrote nothing else.
		if len(p.State) != 1 {
			t.Fatalf("closure %s snapshot has %d keys, want 1 (found) — %v", p.Instance, len(p.State), p.State)
		}
	}

	// The promoter ran twice (once per closure) and wrote project_context into
	// the GLOBAL state both times.
	if got := countKind(e, "state.updated.project_context"); got != 2 {
		t.Fatalf("state.updated.project_context count = %d, want 2 (one promotion per closed request)", got)
	}

	// The global state view reflects the promotion: read it at the last
	// project_context write's span. The promoted value is one of the two cones'
	// found payloads (last writer wins on the project_context key; both writes
	// share the key so the global kv has it). Assert the key is present and its
	// value is a real found payload (a path field).
	lastSpan := lastSpanOfKind(e, "state.updated.project_context")
	if lastSpan == "" {
		t.Fatal("no state.updated.project_context on the log")
	}
	pe := newProjectionEval(collect(e), e.liveDecls(), e.rebuildScopes(e.liveSubscribers()))
	_, events, ok := pe.matched("global_state", lastSpan)
	if !ok {
		t.Fatal("global_state projection did not resolve")
	}
	kv := Projection{Type: TypeKV}.foldKV(events)
	pcRaw, ok := kv.Get("state.updated.project_context")
	if !ok {
		t.Fatalf("global_state has no project_context — keys %v", kv.Keys())
	}
	var pc struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(pcRaw), &pc); err != nil || pc.Path != "main.go" {
		t.Fatalf("global_state[project_context] = %s (want a found payload with path main.go)", pcRaw)
	}

	// The two request states did not leak into each other: the per-scope state of
	// each request instance holds exactly its own found (already asserted via the
	// closure snapshots above; this re-checks via the built-in scopeState fold).
	srAfter := e.rebuildScopes(e.liveSubscribers())
	peAfter := newProjectionEval(collect(e), e.liveDecls(), srAfter)
	var reqInstances []string
	for k := range srAfter.instances {
		if srAfter.instances[k].name == "request" {
			reqInstances = append(reqInstances, srAfter.instances[k].rootSpan)
		}
	}
	sort.Strings(reqInstances)
	if len(reqInstances) != 2 {
		t.Fatalf("expected 2 request instances, got %d", len(reqInstances))
	}
	for _, root := range reqInstances {
		st := peAfter.scopeState("request", root)
		if len(st) != 1 {
			t.Fatalf("request %s state has %d keys, want 1 (found, local) — %v", root, len(st), st)
		}
		var f struct {
			Req string `json:"req"`
		}
		_ = json.Unmarshal([]byte(st["found"]), &f)
		if f.Req != root {
			t.Fatalf("request %s state carries found of %q — cross-cone leak", root, f.Req)
		}
	}
}

// lastSpanOfKind returns the span of the last event with the given kind tail.
func lastSpanOfKind(e *Engine, kind string) string {
	span := ""
	for ev := range e.Events() {
		if _, _, k := splitSubject(ev.Subject); k == kind {
			span = ev.Trace.SpanID
		}
	}
	return span
}
