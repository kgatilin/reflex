package engine

import (
	"encoding/json"
	"strings"
)

// projectionEval is the reference evaluation of a declared Projection (doc 24
// §6): a fold over a causal horizon, computed by a backward walk over caused_by
// from the trigger event's log position to the horizon root. It is the
// *definition* of a view — caches are strategies, the walk is the truth (G1).
// Everything it touches is on Events(); a view is recomputable from the log
// alone (G8). The engine stays payload-blind: it selects matched events by
// subject kind (subjectMatch over Projection.On) and folds by the declared
// Key/Value path tokens, never by payload meaning.
//
// projectionEval is constructed once per Drain (it indexes the log by span)
// and asked for a named projection at a given trigger span — that pairing is
// exactly "evaluated at the trigger's causal position" (doc 24 §6).
type projectionEval struct {
	log    []Event
	bySpan map[string]int        // span id → log index
	byName map[string]Projection // declared projection name → decl
	sr     *scopeRuntime         // for the request horizon (cone membership)
}

// newProjectionEval indexes the log by span and the declared projections by
// name. It folds the Projection decls out of the recorded decls so a node's
// Reads can resolve a name to its declaration.
func newProjectionEval(log []Event, decls []Decl, sr *scopeRuntime) *projectionEval {
	bySpan := make(map[string]int, len(log))
	for i, ev := range log {
		bySpan[ev.Trace.SpanID] = i
	}
	byName := map[string]Projection{}
	for _, d := range decls {
		if p, ok := d.(Projection); ok {
			byName[p.Name] = p
		}
	}
	return &projectionEval{log: log, bySpan: bySpan, byName: byName, sr: sr}
}

// matched returns the events the named projection folds over, evaluated at the
// trigger span — the backward caused_by walk bounded by the declared horizon,
// returned in LOG ORDER (the deterministic order the kv tie-break and the log
// shape both need, doc 24 §6). An unknown name yields no events (an empty view,
// the same null object emptyViews returned before).
func (pe *projectionEval) matched(name, triggerSpan string) (Projection, []Event, bool) {
	p, ok := pe.byName[name]
	if !ok {
		return Projection{}, nil, false
	}
	cone := pe.horizonCone(p.In, triggerSpan)
	var out []Event
	for _, idx := range cone {
		ev := pe.log[idx]
		_, _, kind := splitSubject(ev.Subject)
		if matchAny(p.On, ev.Subject, kind) {
			out = append(out, ev)
		}
	}
	return p, out, true
}

// matchAny reports whether any of the patterns matches the event. A projection
// On pattern is matched against BOTH the kind tail (the usual handler-desugar
// match, like a node's On) AND the full subject — so a global-horizon
// projection can name a fully-qualified subject (e.g. "sys.state.updated.X")
// while a request-horizon projection names a bare kind ("state.updated.goal").
// Matching either is deliberate: the engine selects by subject tokens only.
func matchAny(patterns []string, subject, kind string) bool {
	for _, pat := range patterns {
		if subjectMatch(pat, kind) || subjectMatch(pat, subject) {
			return true
		}
	}
	return false
}

// horizonCone returns the log indices (ascending) the projection's horizon
// admits, evaluated at the trigger span:
//
//   - request: the backward caused_by walk from the trigger, bounded at the
//     covering request-scope instance root. This is exactly the request cone
//     the trigger lives in (doc 24 §6 / 26 §2a): N parallel request cones are N
//     disjoint walks, so a read sees only its own cone's facts — the isolation
//     guarantee. If the trigger is in no request cone, the walk still bounds at
//     the trigger's own causal roots (best available cone = the full chain).
//   - global: log order over ALL events, outside any cone (doc 24 §6 / topology
//     HorizonGlobal). The behavioural-fact / catalog horizon.
//   - session: the backward caused_by walk bounded by matching SessionID — the
//     best available approximation until the resolver chains requests into one
//     session cone (doc 24 §6 note; see sessionCone).
func (pe *projectionEval) horizonCone(h Horizon, triggerSpan string) []int {
	switch h {
	case HorizonGlobal:
		idxs := make([]int, len(pe.log))
		for i := range pe.log {
			idxs[i] = i
		}
		return idxs
	case HorizonSession:
		return pe.sessionCone(triggerSpan)
	default: // HorizonRequest and the empty default
		return pe.requestCone(triggerSpan)
	}
}

// requestCone returns the trigger's covering request CONE — every event that is
// a member of the trigger's narrowest request instance (doc 26 §2a: "stops at
// the trigger's covering request scope instance root"). The cone is the scope
// membership maintained in scope.go (the materialised caused_by descent of doc
// 24 §5), NOT merely the trigger's ancestor chain: a sibling state write (caused
// by the same root, not by the trigger) is in the cone and must be visible. Two
// sibling request cones share no span (single-cause descent partitions the DAG),
// so two concurrent triggers yield disjoint cones — read-at-trigger isolation.
//
// Fallback: if the trigger is in no request instance (no covering request
// scope), the cone is the trigger's backward caused_by walk to its roots — the
// best available cone when no request horizon exists.
func (pe *projectionEval) requestCone(triggerSpan string) []int {
	if pe.sr != nil {
		root := pe.sr.narrowestRequest(triggerSpan)
		if root != "" {
			return pe.membersOf(instanceKey("request", root))
		}
	}
	return pe.walk(triggerSpan, func(string) bool { return false })
}

// membersOf returns the log indices (ascending) of every event whose scope
// membership includes the instance key — the cone of that instance (doc 24 §5).
func (pe *projectionEval) membersOf(key string) []int {
	var idxs []int
	if pe.sr == nil {
		return idxs
	}
	for i, ev := range pe.log {
		if memberOf(pe.sr.membership[ev.Trace.SpanID], key) {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// sessionCone is the session-horizon approximation. The intended horizon is the
// whole session chain (the resolver chaining each request to the previous
// closure, doc 24 §6); that chaining does not exist yet, so this walks the
// trigger's caused_by chain to its roots, keeping only spans whose event shares
// the trigger's SessionID. For a single-request session this equals the request
// cone plus session-class siblings; across requests it under-approximates
// (it cannot cross a caused_by gap the resolver has not yet bridged). Documented
// approximation — request and global are exact.
func (pe *projectionEval) sessionCone(triggerSpan string) []int {
	idx, ok := pe.bySpan[triggerSpan]
	if !ok {
		return nil
	}
	want := pe.log[idx].Trace.SessionID
	all := pe.walk(triggerSpan, func(string) bool { return false })
	if want == "" {
		return all
	}
	var out []int
	for _, i := range all {
		if pe.log[i].Trace.SessionID == want {
			out = append(out, i)
		}
	}
	return out
}

// walk performs the backward caused_by descent from start, returning the log
// indices of every reachable span in ASCENDING log order (the deterministic
// fold order). stopAt(span), when true for a span, includes that span but does
// not descend into its causes — the horizon boundary. The walk is acyclic by
// construction (caused_by points strictly backward in log order), so a visited
// set is sufficient and termination is guaranteed.
func (pe *projectionEval) walk(start string, stopAt func(span string) bool) []int {
	seen := map[string]struct{}{}
	var idxs []int
	var visit func(span string)
	visit = func(span string) {
		if _, done := seen[span]; done {
			return
		}
		seen[span] = struct{}{}
		idx, ok := pe.bySpan[span]
		if !ok {
			return
		}
		idxs = append(idxs, idx)
		if stopAt(span) {
			return
		}
		for _, cause := range pe.log[idx].Trace.CausedBy {
			visit(cause)
		}
	}
	visit(start)
	sortInts(idxs)
	return idxs
}

// sortInts sorts a small int slice ascending (insertion sort; cones are small
// and this avoids a sort import for one call site). Ascending log index is the
// deterministic fold order (doc 24 §6 last-writer-wins is log order).
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// foldKV reduces matched events into a key→payload map using the declared
// Key/Value path selectors, in log order with last-writer-wins on key ties (the
// single-writer determinism of doc 24 §6). The result is materialised so the
// returned KV is a stable snapshot of the evaluation.
//
// Selection is payload-blind: Key/Value are dotted path tokens the engine reads
// out of the event mechanically. A "payload.*" path indexes into the JSON
// payload; "subject" / "subject.tail.after.<prefix>" select from the subject;
// an empty Key defaults to the subject's kind tail; an empty Value defaults to
// the whole payload (the §2a per-scope state shape: path → payload bytes).
func (p Projection) foldKV(events []Event) projectedKV {
	kv := projectedKV{m: map[string]json.RawMessage{}, order: nil}
	for _, ev := range events {
		key := p.selectKey(ev)
		if key == "" {
			continue
		}
		val := p.selectValue(ev)
		if _, exists := kv.m[key]; !exists {
			kv.order = append(kv.order, key)
		}
		kv.m[key] = val // last writer wins (events arrive in log order)
	}
	return kv
}

// selectKey extracts the kv key for an event per the declared Key path. The
// default (empty Key) is the subject's kind tail — the natural key for the
// per-scope state fold (path → payload, doc 26 §2a), where the path is the
// subject tail after "state.updated.".
func (p Projection) selectKey(ev Event) string {
	if p.Key == "" {
		_, _, kind := splitSubject(ev.Subject)
		return kind
	}
	if v, ok := selectPath(ev, p.Key); ok {
		return jsonToString(v)
	}
	return ""
}

// selectValue extracts the kv value for an event per the declared Value path.
// The default (empty Value) is the whole payload — the per-scope state stores
// the payload bytes under its path (doc 26 §2a, the engine stays payload-blind).
func (p Projection) selectValue(ev Event) json.RawMessage {
	if p.Value == "" {
		if len(ev.Payload) == 0 {
			return json.RawMessage("null")
		}
		return ev.Payload
	}
	if v, ok := selectPath(ev, p.Value); ok {
		return v
	}
	return json.RawMessage("null")
}

// selectPath reads a dotted path out of an event mechanically. The first token
// names the axis: "payload" indexes into the JSON payload by the remaining
// tokens; "subject" returns the whole subject, or with a remaining ">"-less
// tail token the subject's kind tail. Returns the raw JSON value at the path
// and whether it was found. Payload-blind: this is structural JSON navigation,
// not meaning.
func selectPath(ev Event, path string) (json.RawMessage, bool) {
	toks := strings.Split(path, ".")
	switch toks[0] {
	case "subject":
		return json.RawMessage(`"` + jsonEscape(ev.Subject) + `"`), true
	case "kind":
		_, _, kind := splitSubject(ev.Subject)
		return json.RawMessage(`"` + jsonEscape(kind) + `"`), true
	case "payload":
		return navigateJSON(ev.Payload, toks[1:])
	default:
		// Treat a bare path as a payload path (the common case
		// "payload.path" abbreviated to "path" is not used by the example, but
		// being lenient keeps the selector payload-blind and total).
		return navigateJSON(ev.Payload, toks)
	}
}

// navigateJSON walks a JSON object by the given keys and returns the raw value
// at the leaf. A missing key or a non-object mid-walk yields (nil, false).
func navigateJSON(raw json.RawMessage, keys []string) (json.RawMessage, bool) {
	if len(keys) == 0 {
		if len(raw) == 0 {
			return nil, false
		}
		return raw, true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	v, ok := m[keys[0]]
	if !ok {
		return nil, false
	}
	return navigateJSON(v, keys[1:])
}

// jsonToString renders a raw JSON value as a flat string key: a JSON string is
// unquoted, anything else is its compact JSON text. Deterministic and total.
func jsonToString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	// b is "\"...\""; strip the surrounding quotes Marshal added.
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}

// projectedKV is a materialised kv view (doc 24 §6 ShapeKV): a snapshot of the
// fold, satisfying the KV interface. Keys() returns insertion order (log order
// of first write) so iteration is deterministic.
type projectedKV struct {
	m     map[string]json.RawMessage
	order []string
}

func (kv projectedKV) Get(key string) (json.RawMessage, bool) {
	v, ok := kv.m[key]
	return v, ok
}

func (kv projectedKV) Keys() []string {
	out := make([]string, len(kv.order))
	copy(out, kv.order)
	return out
}

// statePathPrefix is the subject-kind prefix of a per-scope state write (doc 26
// §2a): a node's state.updated.{path} fact. The engine keys the per-scope state
// by the {path} tail after this prefix and stores the event payload — keyed by
// the path token, payload-blind on the bytes.
const statePathPrefix = "state.updated."

// scopeState computes the one-state-per-scope built-in fold (doc 26 §2a) for a
// scope instance: the kv folding the cone's state.updated.{path} events into
// path → payload, in log order with last-writer-wins (single-writer
// determinism, doc 24 §6). It is a built-in projection — recomputable from the
// log like the obligation count (G8) — over the cone defined by scope
// membership (the instance the event is a MEMBER of, which is the writer's In
// scope: writes are local, doc 26 §2a). Returns nil when the cone wrote no
// state.
//
// Attribution is "the instance the event is a member of" — for the example
// topology every state-writing node is in:request with no nested state-writing
// sub-scope, so the request state is the fold of all state.updated.* in the
// request cone. A nested state-writing sub-scope's writes would land in the
// NARROWEST covering instance of the writer's In scope; here we attribute a
// write to instanceKey(name, rootSpan) iff the event's membership includes that
// exact instance, which is the writer's own cone — the general "writer's In
// scope" rule, approximated as "member of this instance".
func (pe *projectionEval) scopeState(name, rootSpan string) map[string]json.RawMessage {
	key := instanceKey(name, rootSpan)
	state := map[string]json.RawMessage{}
	order := false
	for _, ev := range pe.log { // log order ⇒ last-writer-wins is correct
		if pe.sr == nil {
			break
		}
		if !memberOf(pe.sr.membership[ev.Trace.SpanID], key) {
			continue
		}
		_, _, kind := splitSubject(ev.Subject)
		if !strings.HasPrefix(kind, statePathPrefix) {
			continue
		}
		path := strings.TrimPrefix(kind, statePathPrefix)
		if path == "" {
			continue
		}
		val := ev.Payload
		if len(val) == 0 {
			val = json.RawMessage("null")
		}
		state[path] = val
		order = true
	}
	if !order {
		return nil
	}
	return state
}

// memberOf reports whether key is in the membership slice.
func memberOf(membership []string, key string) bool {
	for _, k := range membership {
		if k == key {
			return true
		}
	}
	return false
}

// TypeBuilder turns a projection's matched events (selected payload-blind by
// the engine: the backward caused_by walk bounded by the horizon, On-matched,
// in log order) into the view value handed to a reaction (doc 26 §4b). The
// builder is the type-specific shaping layer: "kv"/"log" are payload-blind, a
// richer type (e.g. "llm.history") may read payloads — but it is still a pure
// function of the matched events, so a view stays recomputable from the log
// (G8). It returns `any`; a node reads it type-safely through ViewAs.
type TypeBuilder func(p Projection, events []Event) any

// typeBuilders is the open registry of view types (doc 26 §4b). "kv" and "log"
// are registered here; packages register more in init (nodes/llm →
// "llm.history"). Registration is process-global and static, like the provider
// adapter registry — a collision is a programming error.
var typeBuilders = map[string]TypeBuilder{
	TypeKV:  func(p Projection, events []Event) any { return p.foldKV(events) },
	TypeLog: func(_ Projection, events []Event) any { return append([]Event(nil), events...) },
}

// RegisterType installs a view-type builder under name (doc 26 §4b). Re-using a
// name panics — view-type wiring is static. Packages call this from init so the
// type is available before any topology is applied.
func RegisterType(name string, b TypeBuilder) {
	if _, ok := typeBuilders[name]; ok {
		panic("engine: view type " + name + " already registered")
	}
	typeBuilders[name] = b
}

// typeRegistered reports whether a view type has a registered builder — the
// validator uses it to reject a projection whose Type is unknown (doc 26 §4b).
func typeRegistered(name string) bool {
	if name == "" {
		name = TypeKV // empty defaults to kv
	}
	_, ok := typeBuilders[name]
	return ok
}

// ViewAs resolves a named view and asserts it to T — the type-safe read surface
// a node body uses (doc 26 §4b): `h := engine.ViewAs[llm.History](views, "history")`.
// An unresolved name or a type mismatch yields T's zero value, so a body never
// panics on a missing/misdeclared view (it sees an empty view, the null object).
func ViewAs[T any](views Views, name string) T {
	if t, ok := views.Value(name).(T); ok {
		return t
	}
	var zero T
	return zero
}

// projectionViews is the real Views implementation attached at dispatch (doc 24
// §6 / 26 §4b), replacing emptyViews: Value(name) resolves the named declared
// projection, evaluates its matched events for THIS trigger's causal position,
// and runs the Type's builder. KV/Log are sugar over Value for the two built-in
// types. Names not in the node's Reads are still resolvable (the engine does not
// enforce the Reads allowlist at read time — a node only ever asks for what it
// declared); an unknown name yields nil (an empty view, the null object).
type projectionViews struct {
	eval        *projectionEval
	triggerSpan string
}

func (v projectionViews) Value(name string) any {
	p, events, ok := v.eval.matched(name, v.triggerSpan)
	if !ok {
		return nil
	}
	t := p.Type
	if t == "" {
		t = TypeKV
	}
	b, ok := typeBuilders[t]
	if !ok {
		return nil
	}
	return b(p, events)
}

func (v projectionViews) KV(name string) KV {
	if kv, ok := v.Value(name).(KV); ok {
		return kv
	}
	return emptyKV{}
}

func (v projectionViews) Log(name string) []Event {
	if log, ok := v.Value(name).([]Event); ok {
		return log
	}
	return nil
}
