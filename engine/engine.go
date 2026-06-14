package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
)

// Engine is the kernel: one append-only log, one dispatcher. Its whole
// public surface is the four methods below — append (the sole write),
// apply (the changeset pipeline), drain (dispatch to quiescence), and
// the readable log (no privileged plane, G8: audit, cost, every metric
// is a fold over Events).
type Engine struct {
	log   []Event
	decls []Decl
	// frontier is the index of the next undispatched event in log. Drain
	// advances it as it dispatches; re-running Drain resumes from here, so a
	// crash mid-drain recovers by simply calling Drain again (G5).
	frontier int
}

// New returns an empty engine: no topology, no events. Everything it
// will ever hold arrives through Apply and Append.
func New() *Engine {
	return &Engine{}
}

// Apply runs the changeset pipeline (doc 20 via §7): the resulting graph
// is validated as a whole — not each step — and applied atomically
// between dispatches; intermediate states are inexpressible.
//
// In the doc-27 step-1 milestone this is the validation path only: it folds
// the decls and runs the connectivity validator (Validate). If the topology
// is connected it records the decls and returns nil; otherwise it returns a
// ValidationError carrying the Report. No event is appended and no drain runs
// — fact recording and dispatch belong to later steps. Callers that want the
// Report regardless of connectivity should call Validate directly; that is the
// cleaner read-only surface, and Apply is implemented on top of it.
func (e *Engine) Apply(_ context.Context, decls ...Decl) error {
	rep, err := Validate(decls...)
	if err != nil {
		return err
	}
	if !rep.Connected {
		return &ValidationError{Report: rep}
	}
	e.decls = append(e.decls, decls...)
	return nil
}

// ValidationError reports a topology that failed connectivity validation
// (doc 27 §5). It carries the full Report so the caller can render the gaps
// and the suggested LLM bridges.
type ValidationError struct {
	Report Report
}

func (e *ValidationError) Error() string {
	return "engine: topology is not connected — " +
		"dead-ends, unreachable nodes, fragments, or unbounded cycles present (see Report)"
}

// Append puts one event on the log — the sole write (§4) — and stamps
// it: span id minted, session resolved, request id derived. Adapters and
// the operator surface call this for ingress; reactions never do (their
// Emits are appended by the dispatcher).
func (e *Engine) Append(_ context.Context, subject string, payload json.RawMessage) (Event, error) {
	ev := Event{
		Subject: subject,
		Payload: payload,
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: sessionOf(subject),
			// request_id is the narrowest request-scope covering all causes
			// (§2), derived in Drain's dispatch once cone membership is known
			// (see process). An externally-appended ingress event has no cause
			// and is in no request cone yet, so it is empty here — correct, not
			// pending.
			RequestID: "",
			// CausedBy is empty: an externally-appended event is ingress, it has
			// no cause inside the log (§2 uprightness — causality is a fact of
			// the log, and an ingress event roots its own chain).
			CausedBy: nil,
		},
	}
	e.log = append(e.log, ev)
	return ev, nil
}

// mintSpan returns a deterministic, replay-stable span id: a monotonic
// counter over the current log length (§2: event ≡ span). No time, no
// randomness — the same scenario replayed yields the same ids, which is what
// the acceptance test pins.
func (e *Engine) mintSpan() string {
	return fmt.Sprintf("e%d", len(e.log))
}

// Drain dispatches until every open obligation closes (§5): events fan
// out to the live table with correlation stamped and budgets enforced,
// scope closures fire exactly once per instance, and the call returns at
// quiescence. Crash recovery is re-running Drain — the scope state is a fold
// over the log, recomputed at entry (G5/G8).
//
// Obligation accounting is hierarchical, so dispatch is depth-first (doc 24
// §5): an event entering a cone increments it, its children increment their
// cones BEFORE the parent decrements, and an instance whose count crosses to
// zero closes exactly once (G6). The flat 2a frontier loop only drives the
// roots of that recursion — events at the frontier with no open cone above
// them (ingress) — and a re-drive resumes from the recomputed frontier.
func (e *Engine) Drain(ctx context.Context) error {
	nodes := e.liveNodes()
	sr := e.rebuildScopes(nodes)

	// Drive each undispatched frontier event depth-first to quiescence. process
	// dispatches the event AND its whole caused subtree, appending as it goes;
	// the appended children sit past the frontier, so the loop's own cursor
	// skips over them (process already handled them). Quiescence is the
	// frontier reaching the end of the log with no open obligation.
	for e.frontier < len(e.log) {
		idx := e.frontier
		e.frontier++
		e.process(ctx, idx, nodes, sr)
	}
	return nil
}

// rebuildScopes reconstructs the scope-runtime cache from the log up to the
// frontier (G8: in-memory state is a fold over Events). Every event already
// dispatched has its membership and the instances it rooted recomputed, and
// already-emitted scope.{name}.closed / .budget_exhausted facts mark their
// instances closed/exhausted so a re-drive never re-emits them (G6). Counts
// and obligations of still-open instances are rebuilt so the depth-first pass
// resumes exactly.
func (e *Engine) rebuildScopes(nodes []Node) *scopeRuntime {
	sr := newScopeRuntime(declaredScopes(e.decls), nodes)
	// Replay dispatched events in order, re-rooting and re-stamping membership.
	// Obligations are not replayed (they are an in-flight quantity of a drain
	// in progress); a fresh drain re-derives them as it dispatches. Closed
	// instances are marked so closure is not re-emitted.
	for i := 0; i < e.frontier && i < len(e.log); i++ {
		ev := e.log[i]
		_, scope, kind := splitSubject(ev.Subject)
		sr.admit(ev, scope, kind)
		// A scope.{name}.closed already on the log seals its instance (caused by
		// the instance root): re-mark so a re-drive never re-emits closure (G6).
		if name, reason, _, ok := parseScopeFact(kind); ok && reason == "closed" {
			for _, cause := range ev.Trace.CausedBy {
				if inst := sr.instances[instanceKey(name, cause)]; inst != nil {
					inst.closed = true
				}
			}
		}
	}
	return sr
}

// process dispatches one event depth-first, maintaining the obligation count
// (doc 24 §5). It enters the event into its cones (+1 each), runs the matched
// reactions appending their emits, recursively processes each emitted child
// (so the child's increments land BEFORE this event's decrement — the
// false-zero guard), and finally leaves the cones (−1 each), closing any whose
// count crossed to zero.
func (e *Engine) process(ctx context.Context, idx int, nodes []Node, sr *scopeRuntime) {
	ev := e.log[idx]
	cls, scope, kind := splitSubject(ev.Subject)

	// Budget cap FIRST, on the cones the event inherits by causality (doc 26
	// §3d): if a budgeted parent cone has already reached its ceiling for this
	// kind, the event is INERT — it is on the log (it happened, doc 26 §3e) but
	// the engine does not admit it into the cone at all: it does not root a new
	// scope, does not count toward obligations, and is delivered to no
	// subscriber. Only the graceful budget_exhausted fact fires, once. Rooting
	// before the gate would let a starved loop-trigger open an empty child cone
	// that closes and re-drives the loop forever; the cap must precede rooting.
	if exhausted := sr.gateInherited(ev, kind); exhausted != nil {
		sr.membership[ev.Trace.SpanID] = nil // inert: in no cone
		e.emitScopeFact(ctx, exhausted, "budget_exhausted", kind, sr)
		return
	}

	// Membership + rooting: an event roots its instances and joins its parents'
	// cones before it is counted, so the +1 lands on every covering instance
	// including ones it just opened. admit also records the dispatched count of
	// the bounded kind in each cone (consumed by the gate on the next event).
	sr.admit(ev, scope, kind)
	sr.tallyKind(ev.Trace.SpanID, kind)

	// Stamp request_id now that membership is known (doc 24 §2: narrowest
	// request scope covering all causes). Re-stamp the stored event too so the
	// log carries it.
	if req := sr.narrowestRequest(ev.Trace.SpanID); req != "" {
		ev.Trace.RequestID = req
		e.log[idx].Trace.RequestID = req
	}

	sr.enter(ev.Trace.SpanID)
	e.fanOut(ctx, idx, cls, scope, kind, nodes, sr)

	// Leave the cones; any that reach zero quiesce and close exactly once.
	for _, key := range sr.membership[ev.Trace.SpanID] {
		if sr.leave(key) {
			e.emitClosure(ctx, sr.instances[key], sr)
		}
	}
}

// fanOut runs every matching live node and recursively processes each emit as
// a child of ev. A React error is not fatal (§4 G3): it becomes a non-terminal
// {node}.failed event in the same cone and the drain continues.
func (e *Engine) fanOut(ctx context.Context, idx int, cls, scope, kind string, nodes []Node, sr *scopeRuntime) {
	ev := e.log[idx]
	for _, n := range nodes {
		if !sr.deliver(n, ev.Trace.SpanID, scope, kind) {
			continue
		}
		if n.Body == nil {
			// A sink consumes its trigger and emits nothing (notify, lifecycle,
			// a barrier). The obligation it held is released when its trigger
			// leaves the cone — there is no reaction to run.
			continue
		}
		emits, err := n.Body.React(ctx, ev, emptyViews{})
		if err != nil {
			p, _ := json.Marshal(map[string]string{"error": err.Error()})
			child := e.appendEmit(ev, cls, Emit{Kind: n.Name + ".failed", Payload: p})
			e.process(ctx, child, nodes, sr)
			continue
		}
		for _, em := range emits {
			child := e.appendEmit(ev, cls, em)
			e.process(ctx, child, nodes, sr)
		}
	}
}

// appendEmit places an emit on the log as a caused event and returns its log
// index. The dispatcher owns scope placement (§2 uprightness): the new subject
// is the emit's kind under the triggering event's class+scope, never a scope
// the reaction chose. Trace inherits the session, mints a fresh span, and
// records the trigger as the single cause. The frontier is advanced past it
// because process drives the child directly (depth-first), not the Drain loop.
func (e *Engine) appendEmit(trigger Event, cls string, em Emit) int {
	idx := len(e.log)
	ev := Event{
		Subject: placeSubject(cls, em.Kind),
		Payload: em.Payload,
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: trigger.Trace.SessionID,
			RequestID: trigger.Trace.RequestID, // narrowed in process once membership is known
			CausedBy:  []string{trigger.Trace.SpanID},
		},
	}
	e.log = append(e.log, ev)
	if e.frontier <= idx {
		e.frontier = idx + 1
	}
	return idx
}

// emitClosure drops scope.{name}.closed for a quiesced instance exactly once
// (G6), placed in the cone's parent (doc 26 §2 sealing). The closed event
// carries the instance id so a join/barrier/bridge consumer can correlate, and
// it is itself dispatched depth-first so its parent-cone consumers run within
// the same drain.
func (e *Engine) emitClosure(ctx context.Context, inst *scopeInstance, sr *scopeRuntime) {
	e.emitScopeFact(ctx, inst, "closed", "", sr)
}

// emitScopeFact appends a scope.{name}.{kind} fact (closed | budget_exhausted)
// caused by the instance's root, placed in the cone's parent class+scope so
// the membership rule keeps it outside the just-sealed cone, and drives it
// depth-first so its consumers fire in this drain. The instance id rides in
// the payload (engine stays payload-blind on the way out; consumers read it).
// session/request_id are left for process to stamp from the recomputed
// membership — the fact lands in the parent cone by the sealing rule, so its
// request_id is the parent's, derived not guessed.
func (e *Engine) emitScopeFact(ctx context.Context, inst *scopeInstance, reason, kind string, sr *scopeRuntime) {
	idx := len(e.log)
	subjectKind := "scope." + inst.name + "." + reason
	ev := Event{
		Subject: placeSubject(inst.placeClass, subjectKind),
		Payload: mustMarshal(closedPayload{
			Instance: inst.rootSpan,
			Scope:    inst.name,
			Reason:   reason,
			Kind:     kind,
		}),
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: inst.placeSession,
			CausedBy:  []string{inst.rootSpan},
		},
	}
	e.log = append(e.log, ev)
	if e.frontier <= idx {
		e.frontier = idx + 1
	}
	e.process(ctx, idx, sr.nodes, sr)
}

// liveNodes returns the reaction nodes from the recorded decls, with scope
// defaulting applied (empty In → "global", matching Validate's foldNodes). The
// live table Drain dispatches against is exactly the validated topology Apply
// stored.
func (e *Engine) liveNodes() []Node {
	return foldNodes(e.decls)
}

// nodeMatches reports whether a node subscribes to an event with the given
// scope token and kind. A node matches iff (a) its scope qualifier admits the
// event's scope and (b) some On pattern matches the kind after handler desugar
// (§2: the node binds the kind, the bus prepended the scope wildcard, so we
// match On against the kind tail alone).
func nodeMatches(n Node, scope, kind string) bool {
	if !scopeAdmits(n.In, scope) {
		return false
	}
	for _, pat := range n.On {
		if subjectMatch(pat, kind) {
			return true
		}
	}
	return false
}

// scopeAdmits applies the node's In qualifier (§5). "global" (the default)
// admits any scope; otherwise the event's scope token must equal In.
//
// 2a simplification: scope is matched as a single equality on the scope token
// (e.g. In == "session"), not the full per-instance cone membership of §5 —
// instances and cone delivery are stage 2b. A session-class event carries
// scope token "session"; sys/ingress events carry "" and are admitted only by
// a "global" node.
func scopeAdmits(in, scope string) bool {
	if in == "" || in == "global" {
		return true
	}
	return in == scope
}

// emptyViews is the 2a read surface: no projections exist yet, so every view
// is empty. Stage 2b/§6 attaches the declared projections evaluated at the
// trigger's causal position.
type emptyViews struct{}

func (emptyViews) KV(string) KV       { return emptyKV{} }
func (emptyViews) Log(string) []Event { return nil }

type emptyKV struct{}

func (emptyKV) Get(string) (json.RawMessage, bool) { return nil, false }
func (emptyKV) Keys() []string                     { return nil }

// Events is the log, readable in order. Every consumer that is not a
// wired Reaction — audit, cost accounting, the trace workbench, tests —
// is a fold over this sequence; none of them get a privileged surface.
func (e *Engine) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for _, ev := range e.log {
			if !yield(ev) {
				return
			}
		}
	}
}
