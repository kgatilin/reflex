# 16 — Engine architecture: mechanisms and guarantees (DRAFT)

> **Status: DRAFT / proposed.** Companion to
> [15-primitive-reduction.md](./15-primitive-reduction.md): that document
> shrinks the *user* vocabulary to two reaction bodies (`llm` + `tool`); this
> one specifies the other side of the contract — what the **engine** provides
> and guarantees, independent of any node vocabulary. Builds on
> [11](./11-domain-model.md) (the three concepts),
> [13](./13-event-taxonomy.md) (envelope), [05](./05-control-plane-as-events.md)
> (control plane). Where current code disagrees, the deltas section closes the
> document.

## Thesis

The engine is **two mechanisms and one consulted projection**:

```
append     put an event on the log (sole writer of the log)
dispatch   fan the event out to the live subscription table —
           consulting the progress projection before delivery
```

Everything else the system appears to "have" — scopes, barriers, budgets,
request correlation, retry, crash recovery, the tool menu, metrics — is
**derived**: a fold over the log, announced back onto the log as events. The
`llm` and `tool` bodies of doc 15 are *clients* of this engine, not part of
it. If doc 15 is "the user writes almost nothing", this document is "because
the engine derives almost everything".

The richness of the system does not live in the node vocabulary. It lives in
the **progress projection over the `caused_by` DAG** — that is where loops,
fan-out joins, cancellation, deadlines, budgets, and orphan detection all
come from. This document is the specification of that machinery.

## The two mechanisms

### Append

The log is append-only and is the single source of truth
(11-domain-model.md, invariant: *anything that cannot be recomputed from the
log is a bug*). `append` is the only write. An event enters the log with the
four-part envelope of doc 13 (`subject`, `trace`, `terminal`, `payload`);
the event id is assigned at append and doubles as the OTel `span_id`.

### Dispatch

`dispatch` fans an appended event out to the subscribers in the live table
(doc 05 — the table is itself a fold of control-plane events). Dispatch does
three things beyond routing:

1. **Stamps correlation.** The dispatcher is the *sole writer* of
   `trace.request_id` and `trace.session_id`, by the
   narrowest-scope-covering-all-causes rule of doc 13. No handler ever
   writes them; both stay recomputable from `caused_by`.
2. **Consults the progress projection before delivery.** Delivery into a
   cone that contains a cancellation, or whose scope deadline has passed,
   is refused. This is the one place a projection feeds back into the
   mechanism (the "one concession" of doc 11) — the projection remains a
   view, but a view the bus reads at delivery time.
3. **Enforces budgets.** A scope whose budget for a kind is exhausted gets
   no further dispatches of that kind into its cone; the refusal is
   announced as `scope.budget_exhausted` (T) `(was loop.exhausted)`. A
   subscription that would close a cycle not covered by any budget is
   refused at wiring time (boot or runtime, same Tarjan check — doc 05).

A handler error during dispatch is **not fatal**: the engine appends
`{node}.failed` (non-terminal) into the cone and continues. Failed branches
are branches containing a failure event; nothing unwinds.

## The progress projection

The engine's heart. A fold of **causal structure only** — it never reads
payloads:

```
frontier(cone)   = non-terminal events in the cone with no descendants
quiescent(cone)  = frontier(cone) is empty
```

Quiescence is Dijkstra–Scholten termination detection; the terminal-event
invariant of doc 11 *is* this algorithm. The formula above is the
*log-level* view; the *operational* mechanism must count obligations
(dispatched-but-uncompleted handler invocations), or it cannot distinguish
"handlers still running" from "all handlers returned nothing" (an orphan) —
see [17-quiescence-prior-art.md](./17-quiescence-prior-art.md) for the
counting scheme, taken from Naiad's progress tracking. Three engine facilities are views of
this one fold:

- **The execution position.** There is no program counter; "where execution
  is" *is* the frontier. The dispatcher's in-memory queue is a cache of the
  frontier — losing it loses nothing (see G5 below).
- **Scopes.** A scope is a cut point in the DAG: a root event plus the cone
  it dominates. Closure is a predicate over the projection.
- **Budgets.** Counting `llm.completed` in a cone is the same fold with a
  filter; `scope.budget_low` is its threshold announcement (12 F1).

### Scopes: rooting and membership

A scope is rooted at an event `R`; its members are `R`'s causal descendants:

```
E ∈ scope(R)  ⟺  R is an ancestor of E
               and the path R → E does not pass through scope.closed(R)
               and no other scope root lies on the path R → E
```

The second clause is the **sealing rule**: `scope.closed` is a boundary
node through which causality *exits* the scope. Without it the model
self-invalidates — `scope.closed.caused_by[]` is the cone's frontier, so
every reaction to a closure would formally be a descendant of the root and
would re-open the scope it just observed closing. With it, the consumer of
`scope.closed` lives in the *parent* scope by construction (exactly
structured concurrency: a join's continuation belongs to the enclosing
block, not the finished one), and doc 15's "turn N+1 never emits back into
the closed cone" is geometry, not discipline.

The third clause makes nesting a *partition*: an event belongs directly to
its nearest enclosing scope. **Nesting is transitive quiescence, for free
from the geometry**: a nested scope's `scope.closed` is itself a non-terminal
event in the parent's cone, so the parent cannot quiesce until every nested
scope has closed. No nesting mechanism exists — only the membership rule.

Events are never *addressed* to a scope — they **land** in one, by
causality alone. `caused_by` is engine-stamped (doc 17), so an emit is
always a child of its trigger and lives wherever the trigger lives; no
handler can place an event in a scope of its choosing. Membership is a
fact of the log, not a sender decision — which is what keeps every scope
property derivable.

How a scope gets rooted is the main open fork (also flagged in doc 15):

- **Declared** — a `scopes:` block names root kinds
  (`root: request.received`, doc 11). Explicit, statically analyzable,
  more config.
- **Auto-rooted** — the engine roots a sub-scope at every fan-out point (an
  event with more than one child). Zero config — an `llm` turn that emits N
  tool calls roots a scope without anyone declaring it — but the scope set
  is only known at runtime.

Leaning: **both, layered** — auto-root at fan-out gives the barrier
behaviour of doc 15 with no config (N=1 degenerates correctly: one child,
one result, one `scope.closed`); a `scopes:` block *additionally* roots
scopes that need a deadline, a budget, or a non-default closure predicate.
Declared scopes are the only ones that carry configuration; auto-rooted
scopes exist purely to emit `scope.closed`.

### The closure algebra — synchronization as predicates

This is the engine's synchronization vocabulary — the analogue of Go's
`sync` package, but on the bus, and **none of it is a node type**. A scope
declares `closed_when`, a predicate over the progress projection of its
cone. The engine announces the threshold crossing as `scope.closed` —
exactly once per scope (G6):

| Concurrency idiom | `closed_when` | on close |
|---|---|---|
| `WaitGroup.Wait` (default) | `quiescent` | — |
| `errgroup` (first error wins) | `quiescent OR any *.failed` | cancel the remaining cone |
| race (first result wins) | `any child terminal` | cancel the remaining cone |
| quorum (N of M) | `count(child terminal) >= N` | cancel the remaining cone |

Two deliberate stances:

1. **`quiescent` is the default and the failure-tolerant one.** Doc 11
   *rejects* failure escalation: errors are non-terminal events, so a
   `tool.b.failed` does not stall or abort the barrier — the cone still
   quiesces, the failure sits in the closed cone's fold, and the consumer
   of `scope.closed` (typically the `llm`) sees it and decides. The
   errgroup row is therefore **opt-in, never implicit**: an author who
   wants first-error-cancels semantics writes the predicate and accepts
   that cancelled branches end as refused deliveries, not as handled work.
   Corollary: every predicate that can close *before* quiescence **must**
   cancel the remaining cone — a closed scope is sealed (membership rule),
   so a straggler result arriving later is appended to the log but refused
   dispatch (G9). Early closure without cancellation is not expressible.
2. **The predicates read structure, not payloads.** `count`, `any`,
   `quiescent` are over event kinds, terminality, and the DAG. A condition
   over payload *values* is not a closure predicate — that is a reaction's
   job (subscribe, inspect, emit).

`scope.closed.caused_by[]` is the cone's frontier at closure — N causes, one
event. It is the causal join (OTel: one span with N links, doc 13), which is
why nothing upstream of a barrier is ever orphaned: every result has the
closure as its descendant.

### Cancellation and deadlines

The cancel half of structured concurrency — `ctx.Done()`, flowing down the
cone:

- **Sources.** Exactly two, both deliberate: a scope's `deadline` elapsing
  (the engine emits `scope.deadline_reached` into the cone) and an explicit
  cancel event (user abort, or a closure predicate's "cancel the rest"
  action). Handler errors are *never* a cancellation source (doc 11).
- **Propagation.** Cancellation is not delivered to running work — it is a
  fact in the cone that the *dispatcher* reads: any event whose cone
  contains a cancellation, or whose enclosing declared scope is past
  deadline, is refused delivery. Refusal is recorded (the event is
  appended, its dispatch is not performed), so the log shows what was cut.
- **Confinement.** Cancellation reaches exactly the cancelled scope's cone.
  Sibling scopes, the parent's other branches, and other requests are
  untouched — the membership rule above is also the blast-radius rule.
- **In-flight effects.** A tool already executing is not interrupted; its
  result is appended (the log records what happened) but not dispatched
  into the cancelled cone. Idempotency-keyed effects (G5) make this safe.

### Budgets — and loops are budgets

The guillotine problem (12 F1): a hard cap that silently discards work. The
engine's shape for any quantitative limit (loop iterations, token spend,
wall clock):

```
budget   = a counting fold over the cone (a projection)
warning  = a threshold event into the cone (scope.budget_low),
           early enough for the consumer to wrap up
backstop = the hard mechanism (refused dispatch / scope.budget_exhausted)
```

Soft limit as information, hard limit as mechanism, always in that order.
The model sees `scope.budget_low` in its transcript fold and can choose to
answer with what it has; the guillotine remains for the case where it
doesn't.

A budget is declared on a scope, per event kind (plus wall clock), and is
maintained by the same incremental counters that detect quiescence
(doc 17) — one mechanism, filtered by kind:

```yaml
scopes:
  - root: request.received
    budget:
      llm.completed: 20        # at most 20 model calls in this cone
      tool.search.call: 10     # this tool at most 10 times
      wall_clock: 120s
```

**This dissolves the loop cap as a separate concept.** In cone geometry a
subscription-graph cycle never exists at runtime — each iteration is fresh
DAG nodes — so "bound the loop" *means* "bound the count of a kind within a
cone". `loop: max_iterations` on a subscription edge is sugar for a budget
on the corresponding scope, declared at the wrong site (the limit belongs
to the region of work, not the wiring); `loop.exhausted` is the special
case of `scope.budget_exhausted` where the kind is the cycle's trigger.
Global boundedness then comes from a **mandatory default budget on the
session scope**: every cycle lives inside some budgeted cone, trivially.
The static Tarjan check (doc 05) remains as a *lint* — "this cycle is not
covered by a tight budget" — not as the load-bearing mechanism.

## What the engine emits

The full catalog of engine-produced events — this *is* the `sys` machinery
table of [00-reference](./00-reference.md), organised here by the mechanism
that produces each kind. Nothing below is hand-wired; the engine declares
these producers once. (Per doc 13: lifecycle events about a session's cone
are `app.session.{id}.*`; session-less machinery is `sys.*`.)

| Mechanism | Emits | Meaning |
|---|---|---|
| dispatcher | `sys.event.dispatched` | a fan-out happened (excluded from descendant counts — see G4) |
| dispatcher | `{node}.failed` | a handler errored; non-terminal, drain continues |
| dispatcher | `scope.budget_exhausted` (T) `(was loop.exhausted)` | a cone's budget for a kind ran out — the hard backstop |
| dispatcher (control plane) | `sys.handler.registered` / `.deregistered` · `sys.subscribed` / `.unsubscribed` · `sys.subscription.rejected` | the live table's own change history (doc 05) |
| progress projection | `scope.opened` / `scope.closed` / `scope.deadline_reached` / `scope.budget_low` | scope lifecycle: threshold crossings of the fold, announced as events |
| watcher (post-quiescence) | `event.orphaned` (T) · `request.unhandled` (T) | the orphan invariant made visible (G4) |
| session resolver | `request.received` · `sys.state.updated` (binding) | ingress → scoped request (doc 13) |
| clock | `sys.clock.tick` | time as events; deadlines are folds over ticks |

Reading the table column-wise gives the design rule: **every engine
mechanism announces its decisions as ordinary events on the same log it
serves.** The engine has no private channel to report through — audit,
debugging, and analysis subscribe to these kinds like any other handler
(doc 01: no privileged plane).

## The guarantees

The contract a topology author can rely on. Each is load-bearing — removing
any one breaks a documented behaviour.

- **G1 — Recompute-from-log.** Every engine-held structure (live table,
  frontier cache, scope states, request stamps, session bindings) is a fold
  of the log and is rebuildable from it. Losing in-memory state cannot lose
  information.
- **G2 — No blocking.** There is no synchronous wait primitive at any
  layer. "Waiting" is a subscription (typically to `scope.closed`); the
  engine never parks a handler.
- **G3 — Errors are events.** A handler failure becomes a non-terminal
  `{node}.failed` in the cone; the drain continues. No unwinding, no
  escalation, no scope abort. Recovery is topology (a subscriber on
  `*.failed`), and the non-terminality of errors is what forces recovery to
  be *visible* (→ G4).
- **G4 — No silent dead ends (orphan invariant).** Every non-terminal event
  either acquires a descendant or is announced: `event.orphaned` for a
  dangling event, `request.unhandled` for a request whose cone quiesced
  without a terminal answer. Checked exceptions, derived from the DAG.
- **G5 — Crash recovery ≡ retry (at-least-once).** Resume after a crash =
  recompute the frontier from the log, re-dispatch it. An
  executed-but-unrecorded reaction never happened; replaying it *is* the
  retry mechanism — no separate retry machinery exists (12 F3). The
  corollary is the engine's one demand on effectful tools: external side
  effects carry an idempotency key, with the intent event appended before
  the effect (the log as outbox/WAL).
- **G6 — The barrier fires exactly once.** Per scope, `scope.closed` is
  emitted exactly once, with the cone's frontier as its `caused_by[]`. A
  consumer subscribed to its own sub-scope's closure fires exactly once
  per fan-out, whatever N is — this is the syncgroup guarantee, and it is
  what doc 15's loop rests on.
- **G7 — No unbounded work.** Every cone lives under a budget (the session
  scope carries a mandatory default; narrower scopes refine it), so no
  cycle can run unbounded; exhaustion is visible (`scope.budget_exhausted`,
  preceded by a `scope.budget_low` warning into the cone). The wiring-time
  cycle check (boot and runtime alike) remains as a lint for cycles not
  covered by a tight budget.
- **G8 — No privileged plane.** The engine's own operations — wiring,
  refusals, scope lifecycle, failures — are events on the same log, subject
  to the same subscription, audit, and permission machinery as domain
  events. The boot layer is `append` + `dispatch` and nothing else (doc 05).
- **G9 — Cancellation is confined and recorded.** Cancel/deadline cuts
  exactly one scope's cone, via refused dispatch, never via handler
  interruption; every refusal is reconstructable from the log.

## What the engine deliberately does not provide

The anti-catalog — absences that are design decisions, not gaps:

- **No retry mechanism.** Retry is a subscription on `*.failed` re-emitting
  the call — a capped cycle, validated like any other (doc 11). Crash
  replay (G5) covers the unrecorded-execution case.
- **No error escalation.** A child's failure is never the scope's failure.
  The errgroup idiom exists (closure algebra) but is opt-in per scope.
- **No state store.** State is `state.updated` events plus a fold. The
  engine holds no KV that isn't a projection.
- **No orchestrator and no agent state.** The loop is a chain of per-turn
  sub-scopes (doc 15); the scratchpad is the cone fold; the step counter is
  the cycle cap. No engine component holds "where the agent is".
- **No payload inspection.** The engine reads structure only — subjects,
  terminality, `caused_by`. Closure predicates, routing, stamping: all
  structural. The first payload-reading mechanism would be the first
  domain-aware mechanism, and the end of the domain-blind engine.
- **No synchronous request/reply.** Decomposes into two events and a
  correlation (doc 01), with `scope.closed` as the general join.

## Deltas this implies in current code

1. **Per-scope incremental quiescence.** `DrainQuiesced` is computed once
   at full drain end (`pkg/bus/bus.go`); the closure algebra needs
   per-cut-point, online detection (delta #3 of doc 11, now load-bearing
   for the doc-15 loop). Algorithm: the obligation-count scheme of
   [doc 17](./17-quiescence-prior-art.md).
2. **Frontier rebuild on start.** The in-memory queue is a frontier cache
   but is not yet rebuilt from the log after a crash (12 F3); G5 is
   currently aspirational.
3. **Auto-rooting at fan-out points** does not exist; today's aggregator
   counts `EventDispatched.subscriber_count` instead of consuming
   `scope.closed`.
4. **Cancellation/deadline delivery refusal** is not implemented; scopes
   today have no deadline mechanism at the dispatcher.
5. **`closed_when` algebra**: only implicit `quiescent` exists, and only at
   drain granularity.

## Open / unresolved

- **Auto-root vs declared scopes** — the leaning above (auto-root for
  barriers, declared for configured scopes) needs validation against the
  static analyzer: can doc-04 analysis still bound behaviour when part of
  the scope set is runtime-emergent?
- **Refused-delivery representation.** A cancelled cone's refused dispatches
  must be reconstructable (G9): is refusal a recorded `sys` event per
  refusal, or derivable from cancellation + subscription table with no
  extra record? Leaning derivable (less log noise), but then the audit view
  must compute it.
- **Quorum/race cancellation semantics** interact with in-flight tools:
  "cancel the remaining cone" cannot interrupt an executing plugin, only
  refuse its result's dispatch. Acceptable (the log records the result), but
  the cost model for "wasted" in-flight work at high fan-out is unexamined.
