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
	// crash mid-drain recovers by simply calling Drain again (G5). In 2a the
	// frontier is in-memory; recomputing it from the log is a 2b concern once
	// dispatch facts are recorded.
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
			// TODO(2b): request_id is the narrowest scope covering all causes
			// (§2). For an externally-appended ingress event there is no cause
			// yet, and request-scope resolution is a 2b concern (scopes land
			// then) — left empty here.
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
// quiescence. Crash recovery is re-running Drain — the frontier is
// recomputed from the log (G5).
func (e *Engine) Drain(ctx context.Context) error {
	nodes := e.liveNodes()
	// Dispatch from the frontier to fixpoint. Each dispatched event may append
	// new events (the matched reactions' emits), which extend the log past the
	// frontier; the loop picks them up on a later iteration. Quiescence is the
	// frontier reaching the end of the log with nothing left to dispatch. 2a
	// topologies are acyclic and deterministic, so this terminates; cycle
	// bounding (budgets/obligation counting) is stage 2b.
	for e.frontier < len(e.log) {
		ev := e.log[e.frontier]
		e.frontier++
		e.dispatch(ctx, ev, nodes)
	}
	return nil
}

// dispatch fans one event out to every matching live node, appending each
// reaction's result. A React error is not fatal (§4 G3): it becomes a
// non-terminal {node}.failed event in the flow and the drain continues.
func (e *Engine) dispatch(ctx context.Context, ev Event, nodes []Node) {
	cls, scope, kind := splitSubject(ev.Subject)
	for _, n := range nodes {
		if !nodeMatches(n, scope, kind) {
			continue
		}
		emits, err := n.Body.React(ctx, ev, emptyViews{})
		if err != nil {
			// G3: the error is an event, not control flow. Append a
			// non-terminal {node}.failed into the same class+scope and keep
			// going — nothing unwinds.
			p, _ := json.Marshal(map[string]string{"error": err.Error()})
			e.appendEmit(ev, cls, Emit{Kind: n.Name + ".failed", Payload: p})
			continue
		}
		for _, em := range emits {
			e.appendEmit(ev, cls, em)
		}
	}
}

// appendEmit places an emit on the log as a caused event. The dispatcher owns
// scope placement (§2 uprightness): the new subject is the emit's kind under
// the triggering event's class+scope, never a scope the reaction chose. Trace
// inherits the session, mints a fresh span, and records the trigger as the
// single cause.
func (e *Engine) appendEmit(trigger Event, cls string, em Emit) {
	ev := Event{
		Subject: placeSubject(cls, em.Kind),
		Payload: em.Payload,
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: trigger.Trace.SessionID,
			RequestID: "", // TODO(2b): derived with request scopes.
			CausedBy:  []string{trigger.Trace.SpanID},
		},
	}
	e.log = append(e.log, ev)
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
