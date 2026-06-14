package engine

import (
	"encoding/json"
	"strings"
)

// scopeRuntime is the engine's built-in progress projection (doc 26 §2): the
// per-instance cone state — membership, obligation counts, per-kind counts,
// closure — that scopes reduce to. It is a *cache* of a fold over the log
// (G8): every field here is recomputable from Events() alone (caused_by
// descent + subject kinds), maintained incrementally only for efficiency
// ("caches are strategies, the walk is the definition", doc 24 §5).
//
// Nothing in here is a privileged plane. rebuild() reconstructs the whole
// thing from the log; the incremental updates on the dispatch path are an
// optimisation of that fold, and Drain rebuilds before it runs so a re-drive
// after a crash recovers (G5).
type scopeRuntime struct {
	// instances maps instance id (the root event's span id) → its state. An
	// instance comes into being when a root event is dispatched.
	instances map[string]*scopeInstance

	// membership maps an event's span id → the set of instance ids whose cone
	// it belongs to (an event nests in several cones: node-loop ⊂ request ⊂
	// global). This is the materialised caused_by descent of doc 24 §5.
	membership map[string][]string

	// roots, in declaration order, the two rooting sources (doc 24 §5): a
	// declared Scope (kind-rooted) and a scope-rooting Node (every firing roots
	// an instance of Scope). Both are read off the recorded decls.
	declared []Scope
	nodes    []Subscriber
}

// scopeInstance is one cone's live state (doc 26 §2). All of it is a fold over
// the events caused_by-descended from rootSpan.
type scopeInstance struct {
	name     string // scope name (the subject token, e.g. "request")
	rootSpan string // instance id == root event's span id (replay-stable)

	// placeClass / placeSession are the class prefix and session of the *root*
	// event — where scope.{name}.closed / .budget_exhausted are placed so their
	// consumer lives in this cone's parent (doc 26 §2 sealing row). Using the
	// root event's own prefix keeps the closed fact in the same class+session
	// as the work it seals, while the membership rule (closure is the boundary
	// causality exits through) keeps it out of the just-closed cone — admit
	// excludes a scope.{name}.closed event from the instance it seals.
	placeClass   string
	placeSession string

	budget map[string]int // per-kind ceiling within the cone (nil ⇒ unbounded)

	obligations int            // open-obligation count; quiesces at zero
	counts      map[string]int // per-kind dispatched-count within the cone
	exhausted   map[string]struct{}
	closed      bool
}

func newScopeRuntime(declared []Scope, nodes []Subscriber) *scopeRuntime {
	return &scopeRuntime{
		instances:  map[string]*scopeInstance{},
		membership: map[string][]string{},
		declared:   declared,
		nodes:      nodes,
	}
}

// rootsOf returns the scope instances the event roots, by name. A declared
// Scope roots iff the event's kind matches its Root; a scope-rooting Node roots
// iff that node matches the event (its firing on this event opens the cone).
// The instance id is always the event's span id (doc 24 §5: deterministic,
// replay-stable). Multiple distinct scope *names* may root on one event (then
// each is a separate instance keyed by name+span).
func (sr *scopeRuntime) rootsOf(ev Event, scope, kind string) []rootSpec {
	var out []rootSpec
	for _, s := range sr.declared {
		if s.Root != "" && subjectMatch(s.Root, kind) {
			out = append(out, rootSpec{name: s.Name, budget: s.Budget})
		}
	}
	for _, n := range sr.nodes {
		if n.Scope == "" {
			continue
		}
		if !subscriberMatches(n, scope, kind) {
			continue
		}
		out = append(out, rootSpec{name: n.Scope, budget: budgetForScope(sr.declared, n.Scope)})
	}
	return out
}

// rootSpec names a scope instance to open on a root event.
type rootSpec struct {
	name   string
	budget map[string]int
}

// budgetForScope finds the declared budget for a scope name (a node-rooted
// scope shares its budget config with the same-named declared Scope, doc 24
// §5: "the same named scope with a different root specifier").
func budgetForScope(declared []Scope, name string) map[string]int {
	for _, s := range declared {
		if s.Name == name {
			return s.Budget
		}
	}
	return nil
}

// instanceKey is the deterministic id of a scope instance: name + root span.
// Distinct names rooted on one event are distinct instances; the same name can
// never root twice on one span. The id surfaced to consumers (payload, closed
// correlation) is the root span alone — names live in the subject.
func instanceKey(name, rootSpan string) string { return name + "@" + rootSpan }

// narrowestRequest returns the request-class scope instance id covering the
// event, or "" if none applies (doc 24 §2: request_id is the narrowest scope
// covering all causes). "narrowest" = the instance whose root is deepest in
// the caused_by chain; with single-cause descent that is the last request
// instance the event is a member of, in membership (descent) order.
func (sr *scopeRuntime) narrowestRequest(spanID string) string {
	var req string
	for _, key := range sr.membership[spanID] {
		inst := sr.instances[key]
		if inst != nil && inst.name == "request" {
			req = inst.rootSpan
		}
	}
	return req
}

// closedPayload is the body of a scope.{name}.closed / .budget_exhausted fact:
// the instance id so a join/barrier/bridge consumer can correlate (doc 26 §2),
// plus the closing instance's FINAL STATE SNAPSHOT — the one-state-per-scope kv
// at quiescence (doc 26 §2a). The engine attaches its own maintained fold,
// payload-blind on the way out (exactly as it attaches the obligation-driven
// closure); a parent-scope consumer reads State to fold chosen fields up. This
// is the ONLY request→global promotion path: state flows up through closures,
// never sideways (doc 26 §2a).
type closedPayload struct {
	Instance string `json:"instance"`
	Scope    string `json:"scope"`
	// Reason distinguishes the two closure predicates (doc 26 §3d): "quiescent"
	// (obligations hit zero) or "budget" (a counted kind hit its ceiling).
	Reason string `json:"reason,omitempty"`
	Kind   string `json:"kind,omitempty"`
	// State is the closing cone's per-scope state at close: path → payload bytes
	// (the §2a fold of the cone's state.updated.{path} events). nil when the cone
	// wrote no state. The engine stays payload-blind — it carries the bytes.
	State map[string]json.RawMessage `json:"state,omitempty"`
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// closedPayload is a fixed struct of strings; marshalling cannot fail.
		// A panic here would be an engine bug, not a runtime condition.
		panic("engine: marshal scope fact: " + err.Error())
	}
	return b
}

// admit computes an event's cone membership (the caused_by descent of doc 24
// §5) and opens any instances it roots. Membership = the union of its causes'
// memberships (inherited cones) plus instances it roots itself — with two
// subtractions for the three-clause rule:
//
//   - Sealing (clause 2): if the event IS scope.{name}.closed for instance K,
//     it is the boundary causality exits through, so K is removed from its
//     membership — the closed fact lives in the parent cone, not the sealed
//     one. A closed instance never re-enters anyone's membership either.
//   - Partition is implicit: nested instances all appear, narrowest last, in
//     descent order — narrowestRequest reads the last request-class one.
//
// admit is idempotent per span (rebuild replays it; the dispatch path calls it
// once) so membership is stable across a re-drive.
func (sr *scopeRuntime) admit(ev Event, scope, kind string) {
	span := ev.Trace.SpanID
	if _, done := sr.membership[span]; done {
		return
	}

	// Inherit covering cones from the causes, dropping any already closed
	// (closure is monotone; a closed cone never reopens, doc 26 §2).
	set := map[string]struct{}{}
	var order []string
	add := func(key string) {
		if _, ok := set[key]; ok {
			return
		}
		set[key] = struct{}{}
		order = append(order, key)
	}
	for _, cause := range ev.Trace.CausedBy {
		for _, key := range sr.membership[cause] {
			if inst := sr.instances[key]; inst != nil && !inst.closed {
				add(key)
			}
		}
	}

	// Sealing: a scope.{name}.closed / .budget_exhausted fact exits the instance
	// it seals. The fact is caused by that instance's root span, so the sealed
	// instance key is instanceKey(name, cause). Remove it from this event's
	// membership so the closure (and its downstream) lands in the parent cone
	// (doc 24 §5 clause 2 / doc 26 §2). budget_exhausted seals nothing — it is
	// a fact in the parent, same placement — so it is pruned identically.
	if name, _, _, ok := parseScopeFact(kind); ok {
		for _, cause := range ev.Trace.CausedBy {
			delete(set, instanceKey(name, cause))
		}
		var pruned []string
		for _, key := range order {
			if _, keep := set[key]; keep {
				pruned = append(pruned, key)
			}
		}
		order = pruned
	}

	// Root any instances this event opens; the root event is itself a member.
	for _, rs := range sr.rootsOf(ev, scope, kind) {
		key := instanceKey(rs.name, span)
		if _, exists := sr.instances[key]; !exists {
			sr.instances[key] = &scopeInstance{
				name:         rs.name,
				rootSpan:     span,
				placeClass:   classOf(ev.Subject),
				placeSession: ev.Trace.SessionID,
				budget:       rs.budget,
				counts:       map[string]int{},
				exhausted:    map[string]struct{}{},
			}
		}
		add(key)
	}

	sr.membership[span] = order
}

// deliver reports whether node n should receive the event: its On matches the
// kind AND its In scope-qualifier admits the event by CONE MEMBERSHIP (doc 24
// §5 — the 2b upgrade of the 2a class-token scopeAdmits). "global"/empty admits
// anything; a named In admits the event iff the event is a member of some open
// instance of that scope (the dispatcher's ancestor-scope walk, doc 24 §5
// "scope-qualified subscriptions … a delivery-time filter on the ancestor-scope
// walk"). The kind itself is still matched by On after handler desugar.
func (sr *scopeRuntime) deliver(n Subscriber, span, scope, kind string) bool {
	matched := false
	for _, pat := range n.On {
		if subjectMatch(pat, kind) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	in := n.In
	if in == "" || in == "global" {
		return true
	}
	for _, key := range sr.membership[span] {
		if inst := sr.instances[key]; inst != nil && inst.name == in {
			return true
		}
	}
	return false
}

// enter increments the obligation count of every cone the event belongs to
// (doc 24 §5: dispatch of E → +1 up E's ancestor scope chain). Closed cones do
// not appear in membership, so they are never incremented.
func (sr *scopeRuntime) enter(span string) {
	for _, key := range sr.membership[span] {
		if inst := sr.instances[key]; inst != nil {
			inst.obligations++
		}
	}
}

// leave decrements one cone's obligation and reports whether it just crossed to
// zero AND has not already closed — i.e. this is the quiescence edge that must
// emit scope.closed exactly once (G6). Because a cone-I event can only be
// appended while processing another cone-I event, once the count reaches zero
// it stays zero: synchronous detection is exact (doc 26 §2).
func (sr *scopeRuntime) leave(key string) bool {
	inst := sr.instances[key]
	if inst == nil {
		return false
	}
	inst.obligations--
	if inst.obligations == 0 && !inst.closed {
		inst.closed = true
		return true
	}
	return false
}

// gateInherited is the one per-scope runtime gate (doc 26 §3d cap), evaluated
// on the cones the event inherits by causality — BEFORE the event roots any
// scope of its own. For a budgeted inherited cone bounding kind whose running
// (dispatched) count has reached the ceiling, return that instance on the first
// bite (caller makes the event inert and fires the graceful fact once) and mark
// it exhausted so subsequent over-budget events of that kind in that cone are
// starved silently. Returns nil when nothing is capped (dispatch proceeds).
//
// It is read-only on the count (tallyKind does the increment, at admit time)
// and reads only the subject kind and the cone counts — never payload (the
// engine stays payload-blind).
func (sr *scopeRuntime) gateInherited(ev Event, kind string) *scopeInstance {
	for _, key := range sr.inheritedCones(ev) {
		inst := sr.instances[key]
		if inst == nil || inst.budget == nil {
			continue
		}
		ceiling, bounded := inst.budget[kind]
		if !bounded {
			continue
		}
		if inst.counts[kind] >= ceiling {
			if _, already := inst.exhausted[kind]; already {
				return nil // already fired; starve silently
			}
			inst.exhausted[kind] = struct{}{}
			return inst
		}
	}
	return nil
}

// inheritedCones returns the open instance keys an event lands in by causality
// alone — the union of its causes' memberships minus closed instances and the
// instance a scope.{name}.closed/.budget_exhausted fact seals. This is the
// caused_by descent of doc 24 §5 evaluated WITHOUT the event's own rooting, so
// the budget gate sees the parent cones the event would enter, not the child it
// is about to open.
func (sr *scopeRuntime) inheritedCones(ev Event) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(key string) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	for _, cause := range ev.Trace.CausedBy {
		for _, key := range sr.membership[cause] {
			if inst := sr.instances[key]; inst != nil && !inst.closed {
				add(key)
			}
		}
	}
	_, _, kind := splitSubject(ev.Subject)
	if name, _, _, ok := parseScopeFact(kind); ok {
		for _, cause := range ev.Trace.CausedBy {
			key := instanceKey(name, cause)
			if _, ok := seen[key]; !ok {
				continue
			}
			delete(seen, key)
			var pruned []string
			for _, k := range out {
				if k != key {
					pruned = append(pruned, k)
				}
			}
			out = pruned
		}
	}
	return out
}

// tallyKind records one dispatched occurrence of kind in every budgeted cone
// the event belongs to that bounds it (doc 24 §5 counting fold). Called at
// admit time for an event that passed the gate, so the count reflects only
// admitted (delivered) work — the running total the next event's gate reads.
func (sr *scopeRuntime) tallyKind(span, kind string) {
	for _, key := range sr.membership[span] {
		inst := sr.instances[key]
		if inst == nil || inst.budget == nil {
			continue
		}
		if _, bounded := inst.budget[kind]; bounded {
			inst.counts[kind]++
		}
	}
}

// parseScopeFact recognises an engine-authored scope fact kind
// "scope.{name}.closed" or "scope.{name}.budget_exhausted" and returns the
// scope name and the reason ("closed" / "budget_exhausted"). The instance the
// fact concerns is not in the kind — the caller resolves it from the event's
// cause (the root span), which is exactly the sealing target. The boolean
// reports recognition.
func parseScopeFact(kind string) (name, reason string, _ string, ok bool) {
	toks := splitTokens(kind)
	if len(toks) < 3 || toks[0] != "scope" {
		return "", "", "", false
	}
	reason = toks[len(toks)-1]
	if reason != "closed" && reason != "budget_exhausted" {
		return "", "", "", false
	}
	name = joinTokens(toks[1 : len(toks)-1])
	return name, reason, "", true
}

func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ".")
}

func joinTokens(toks []string) string { return strings.Join(toks, ".") }

// classOf returns the class+scope prefix of a subject (everything but the kind
// tail), via splitSubject — the placement prefix for a scope fact.
func classOf(subject string) string {
	cls, _, _ := splitSubject(subject)
	return cls
}

// declaredScopes collects the Scope decls (the kind-rooted rooting source of
// doc 24 §5). Node-rooted scopes are read from the nodes directly.
func declaredScopes(decls []Decl) []Scope {
	var out []Scope
	for _, d := range decls {
		if s, ok := d.(Scope); ok {
			out = append(out, s)
		}
	}
	return out
}
