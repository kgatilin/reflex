package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// countKind returns how many events on the log have the given kind tail.
func countKind(e *Engine, kind string) int {
	n := 0
	for ev := range e.Events() {
		if _, _, k := splitSubject(ev.Subject); k == kind {
			n++
		}
	}
	return n
}

// scopeFactsOf returns the closedPayloads of every scope.{name}.{reason} fact
// on the log, for assertions on instance correlation.
func scopeFactsOf(e *Engine, kind string) []closedPayload {
	var out []closedPayload
	for ev := range e.Events() {
		if _, _, k := splitSubject(ev.Subject); k == kind {
			var p closedPayload
			_ = json.Unmarshal(ev.Payload, &p)
			out = append(out, p)
		}
	}
	return out
}

// TestDrain_BoundedLoopBudgetCapTerminates is acceptance test 1 (doc 28 Stage
// 2b): a gather↔fs loop covered by a budgeted scope. gather emits
// tool.fs.read.call; fs answers tool.fs.read.result; gather loops on the
// result. The "work" scope budgets tool.fs.read.call at 3, so the cap bites,
// scope.work.budget_exhausted fires once, scope.work.closed fires once, and
// Drain terminates instead of looping forever.
func TestDrain_BoundedLoopBudgetCapTerminates(t *testing.T) {
	ctx := context.Background()
	const budget = 3

	decls := []Decl{
		// work scope: rooted by request.received, budgets the loop kind.
		Scope{
			Name:   "work",
			Root:   "request.received",
			Budget: map[string]int{"tool.fs.read.call": budget},
		},
		// resolver: ingress → request.received (roots the work scope).
		Subscriber{
			Name:  "resolver",
			On:    []string{"test.msg"},
			Emits: []string{"request.received"},
			Body:  emitKind("request.received"),
		},
		// gather: kicks off the loop on request.received, then loops on every
		// fs result by emitting another read call. Unbounded by itself — the
		// scope budget is what stops it (doc 26 §3).
		Subscriber{
			Name:  "gather",
			On:    []string{"request.received", "tool.fs.read.result"},
			In:    "work",
			Emits: []string{"tool.fs.read.call"},
			Body:  emitKind("tool.fs.read.call"),
		},
		// fs (tool): answers each read call with a result.
		Subscriber{
			Name:  "fs",
			On:    []string{"tool.fs.read.call"},
			In:    "work",
			Emits: []string{"tool.fs.read.result"},
			Body:  emitKind("tool.fs.read.result"),
		},
	}

	e := New()
	e.install(decls...)
	if _, err := e.Append(ctx, "app.ingress.test.msg", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The cap admits exactly `budget` read calls into the cone; the loop tries
	// one more (gather fires on the budget-th result), which is starved.
	if got := countKind(e, "tool.fs.read.call"); got != budget+1 {
		t.Fatalf("tool.fs.read.call count = %d, want %d (budget admits %d, the loop emits one more that is starved)", got, budget+1, budget)
	}
	// Exactly `budget` results: only the admitted calls reached fs.
	if got := countKind(e, "tool.fs.read.result"); got != budget {
		t.Fatalf("tool.fs.read.result count = %d, want %d", got, budget)
	}
	// budget_exhausted fires exactly once (first bite), closed exactly once (G6).
	if got := countKind(e, "scope.work.budget_exhausted"); got != 1 {
		t.Fatalf("scope.work.budget_exhausted count = %d, want 1", got)
	}
	if got := countKind(e, "scope.work.closed"); got != 1 {
		t.Fatalf("scope.work.closed count = %d, want 1", got)
	}

	// The closed fact correlates to the request.received root instance.
	closed := scopeFactsOf(e, "scope.work.closed")
	if len(closed) != 1 || closed[0].Scope != "work" || closed[0].Instance == "" {
		t.Fatalf("scope.work.closed payload = %+v, want one fact naming scope work with an instance id", closed)
	}

	// Determinism: replay yields the identical log.
	e2 := New()
	e2.install(decls...)
	if _, err := e2.Append(ctx, "app.ingress.test.msg", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append (replay): %v", err)
	}
	if err := e2.Drain(ctx); err != nil {
		t.Fatalf("Drain (replay): %v", err)
	}
	a, b := collect(e), collect(e2)
	if len(a) != len(b) {
		t.Fatalf("replay produced %d events, first produced %d", len(b), len(a))
	}
	for i := range a {
		if a[i].Subject != b[i].Subject || a[i].Trace.SpanID != b[i].Trace.SpanID {
			t.Fatalf("event %d not replay-stable: %q/%q vs %q/%q", i, a[i].Subject, a[i].Trace.SpanID, b[i].Subject, b[i].Trace.SpanID)
		}
	}
}

// emitN emits n copies of one kind — a fan-out producing N parallel obligations
// in the rooting cone.
func emitN(kind string, n int) ReactionFunc {
	return func(_ context.Context, _ Event, _ Views) ([]Emit, error) {
		out := make([]Emit, n)
		for i := range out {
			out[i] = Emit{Kind: kind, Payload: json.RawMessage(`{}`)}
		}
		return out, nil
	}
}

// TestDrain_JoinBarrierClosesOnceWhenAllResultsIn is acceptance test 3: N tool
// calls in one cone close the cone exactly once when all N results are in. The
// degenerate N=1 (and the truly-degenerate N=0, an instance opening zero
// obligations) close immediately — same algebra, no special case (doc 24 §5).
func TestDrain_JoinBarrierClosesOnceWhenAllResultsIn(t *testing.T) {
	ctx := context.Background()

	run := func(n int) *Engine {
		decls := []Decl{
			// fanout scope rooted by the fan-out event; budget bounds the call
			// kind so the topology has a covered cycle-free cone (here acyclic).
			Scope{Name: "fanout", Root: "fan.out", Budget: map[string]int{"tool.noop.call": n + 1}},
			// dispatcher: ingress → fan.out (roots the fanout scope), emitting N
			// parallel calls inside the cone.
			Subscriber{
				Name:  "dispatcher",
				On:    []string{"test.msg"},
				Emits: []string{"fan.out"},
				Body:  emitKind("fan.out"),
			},
			Subscriber{
				Name:  "fanner",
				On:    []string{"fan.out"},
				In:    "fanout",
				Emits: []string{"tool.noop.call"},
				Body:  emitN("tool.noop.call", n),
			},
			// the tool answers each call; results open no further obligations,
			// so once all N land the cone quiesces.
			Subscriber{
				Name:  "noop",
				On:    []string{"tool.noop.call"},
				In:    "fanout",
				Emits: []string{"tool.noop.result"},
				Body:  emitKind("tool.noop.result"),
			},
			// barrier: consumes the closure in the parent cone (the join's
			// continuation belongs to the enclosing block, doc 24 §5 sealing).
			Subscriber{
				Name: "barrier",
				On:   []string{"scope.fanout.closed"},
			},
		}
		e := New()
		e.install(decls...)
		if _, err := e.Append(ctx, "app.ingress.test.msg", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := e.Drain(ctx); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		return e
	}

	for _, n := range []int{0, 1, 3} {
		e := run(n)
		if got := countKind(e, "tool.noop.call"); got != n {
			t.Fatalf("N=%d: tool.noop.call count = %d, want %d", n, got, n)
		}
		if got := countKind(e, "tool.noop.result"); got != n {
			t.Fatalf("N=%d: tool.noop.result count = %d, want %d", n, got, n)
		}
		// The cone closes exactly once regardless of N (G6) — including N=0,
		// where the instance opens zero obligations and closes instantly.
		if got := countKind(e, "scope.fanout.closed"); got != 1 {
			t.Fatalf("N=%d: scope.fanout.closed count = %d, want exactly 1", n, got)
		}
		// No budget bite: the cap was set above N, so the join is natural
		// quiescence, not forced closure.
		if got := countKind(e, "scope.fanout.budget_exhausted"); got != 0 {
			t.Fatalf("N=%d: scope.fanout.budget_exhausted count = %d, want 0 (closure is by quiescence)", n, got)
		}
		closed := scopeFactsOf(e, "scope.fanout.closed")
		if len(closed) == 1 && closed[0].Reason != "closed" {
			t.Fatalf("N=%d: closed reason = %q, want \"closed\"", n, closed[0].Reason)
		}
	}
}

// TestDrain_StallReDrivesIntoNewChildCone is acceptance test 2: a cone
// quiesces on a non-terminal state (a stall, doc 26 §2); a bridge node
// subscribed to scope.attempt.closed re-drives by emitting the next work event
// (attempt.start), opening a NEW child cone — not reopening the closed one
// (closure is monotone). The follow-up loop scope.closed → bridge → re-drive →
// scope.closed is itself a cycle (doc 26 §3f), bounded here by an outer
// "session" scope that budgets attempt.start. The whole thing converges.
func TestDrain_StallReDrivesIntoNewChildCone(t *testing.T) {
	ctx := context.Background()
	const maxAttempts = 3

	decls := []Decl{
		// session: the outer cone that bounds the re-drive loop (doc 26 §3f).
		// Budgeting attempt.start caps how many attempt cones the bridge can
		// open, so the stall→bridge→re-drive cycle terminates.
		Scope{Name: "session", Root: "session.open", Budget: map[string]int{"attempt.start": maxAttempts}},
		// attempt: the inner cone, one per try. It does a unit of work that
		// quiesces on a non-terminal value (no terminal emitted) — a stall.
		Scope{Name: "attempt", Root: "attempt.start"},

		// opener: ingress → session.open (roots session), then kicks the first
		// attempt by emitting attempt.start inside the session cone.
		Subscriber{
			Name:  "opener",
			On:    []string{"test.msg"},
			Emits: []string{"session.open"},
			Body:  emitKind("session.open"),
		},
		Subscriber{
			Name:  "kick",
			On:    []string{"session.open"},
			In:    "session",
			Emits: []string{"attempt.start"},
			Body:  emitKind("attempt.start"),
		},
		// worker: inside an attempt cone, does a tool call; the result opens no
		// terminal, so the attempt cone quiesces non-terminally (a stall).
		Subscriber{
			Name:  "worker",
			On:    []string{"attempt.start"},
			In:    "attempt",
			Emits: []string{"tool.noop.call"},
			Body:  emitKind("tool.noop.call"),
		},
		Subscriber{
			Name:  "noop",
			On:    []string{"tool.noop.call"},
			In:    "attempt",
			Emits: []string{"tool.noop.result"},
			Body:  emitKind("tool.noop.result"),
		},
		// bridge: the stall's continuation. It consumes scope.attempt.closed in
		// the parent (session) cone and re-drives — emits another attempt.start,
		// opening a NEW attempt instance. Bounded by the session budget on
		// attempt.start, so the re-drive loop terminates.
		Subscriber{
			Name:  "bridge",
			On:    []string{"scope.attempt.closed"},
			In:    "session",
			Emits: []string{"attempt.start"},
			Body:  emitKind("attempt.start"),
		},
	}

	e := New()
	e.install(decls...)
	if _, err := e.Append(ctx, "app.ingress.test.msg", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The session budget admits exactly maxAttempts attempt.start events; the
	// bridge tries one more after the last attempt closes, which is starved.
	if got := countKind(e, "attempt.start"); got != maxAttempts+1 {
		t.Fatalf("attempt.start count = %d, want %d (budget admits %d, bridge emits one more that is starved)", got, maxAttempts+1, maxAttempts)
	}
	// Each admitted attempt opens one cone that closes exactly once — so
	// maxAttempts distinct attempt closures, each a distinct instance.
	attemptClosed := scopeFactsOf(e, "scope.attempt.closed")
	if len(attemptClosed) != maxAttempts {
		t.Fatalf("scope.attempt.closed count = %d, want %d (one per admitted attempt)", len(attemptClosed), maxAttempts)
	}
	seen := map[string]int{}
	for _, p := range attemptClosed {
		seen[p.Instance]++
	}
	if len(seen) != maxAttempts {
		t.Fatalf("expected %d DISTINCT attempt instances, got %d (%v) — re-drive must open new child cones, not reopen", maxAttempts, len(seen), seen)
	}
	for inst, n := range seen {
		if n != 1 {
			t.Fatalf("attempt instance %q closed %d times, want exactly 1 (closure is monotone, G6)", inst, n)
		}
	}
	// The session budget bit once; the session cone itself closes exactly once.
	if got := countKind(e, "scope.session.budget_exhausted"); got != 1 {
		t.Fatalf("scope.session.budget_exhausted count = %d, want 1", got)
	}
	if got := countKind(e, "scope.session.closed"); got != 1 {
		t.Fatalf("scope.session.closed count = %d, want 1 (the whole thing converges)", got)
	}
}
