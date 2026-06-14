package engine

import (
	"context"
	"encoding/json"
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
func (e *Engine) Append(ctx context.Context, subject string, payload json.RawMessage) (Event, error) {
	panic("engine: Append not implemented — skeleton for API review")
}

// Drain dispatches until every open obligation closes (§5): events fan
// out to the live table with correlation stamped and budgets enforced,
// scope closures fire exactly once per instance, and the call returns at
// quiescence. Crash recovery is re-running Drain — the frontier is
// recomputed from the log (G5).
func (e *Engine) Drain(ctx context.Context) error {
	panic("engine: Drain not implemented — skeleton for API review")
}

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
