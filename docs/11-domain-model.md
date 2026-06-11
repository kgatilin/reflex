# 11 — Domain model: two primitives, one derived concept

Distilled from a design session on what reflex's concepts actually are.
Everything in this document is normative for new work; where current code
disagrees, the code is the thing to change (a list of those deltas closes
the document).

## The model

```
Event      a record on the append-only log: subject-style type,
           causal links (caused_by[]), terminal flag
Reaction   a pure function (event, log) -> events; the LLM, tools,
           projectors, aggregators are all Reactions — no exceptions
Projection a fold(log) -> view; threshold crossings are announced
           back onto the log as events
           ├── state:    folds state.* payloads   -> "what we know"
           └── progress: folds causal structure   -> "what is in flight"
```

One invariant, never violated: **anything that cannot be recomputed from
the log is a bug.** Session state, drain status, subscription tables —
all views, never stores.

There is no fourth concept. Everything else is a *convention* over these
three:

| Thing | What it is |
|---|---|
| Errors | `*.failed` events, **non-terminal** |
| State writes | `state.updated{path, value}` events, **terminal** |
| Retry / fallback | topology: a capped cycle / an LLM reaction on `*.failed` |
| Scopes (drains) | a standing query: root selector + `closed_when` + `deadline` |
| Barriers / phases / aggregation | a subscription to a sub-scope's `scope.closed` |
| Time | `clock.*` events injected by the runtime |
| The LLM | a Reaction filling graph gaps, injected at compile time |

## Subject-style event types

Event types are hierarchical subjects (NATS-style): `tool.calc.call`,
`tool.calc.result`, `tool.calc.failed`, `llm.turn`, `llm.completed`,
`state.updated`, `scope.closed`. Routing lives in the *type*, never in
the payload — `ToolCallProposed{tool: "calc"}` is payload-encoded RPC
and is deprecated thinking. Consequences:

- Subscription matching is the router. No dispatcher logic, no filters.
- A "data-dependent dead end" (unknown tool name) becomes a *type-level*
  gap the static graph sees: `tool.frobnicate.call` with no consumer.
- Wildcard subscriptions (`tool.*.result`) stay statically analyzable
  because the type universe is closed by declared Emits sets.
- The tool schema handed to an LLM node is a *projection of the
  subscription table*: enumerate consumers of `tool.*.call`. Adding a
  subscriber teaches the model a new tool with zero config.

## Errors are events, not control flow

A handler failure must not abort the drain. The dispatcher emits
`{node}.failed{error}` into the cone and keeps going. Failed branches
are just branches containing a failure event.

Error events are **non-terminal**. This is load-bearing: a terminal
failure would close its branch silently and quiescence would look
healthy. Non-terminal means the orphan invariant demands a reactor —
every error is either handled or *visibly* orphaned (`EventOrphaned`).
Checked exceptions for free.

Retry is then topology: a subscriber on `tool.X.failed` re-emitting
`tool.X.call` forms a cycle, must declare `loop: max_iterations`, and
the retry budget is validated statically by Tarjan like any other
cycle. Smart fallback is an LLM reaction subscribed to `*.failed`.
Hard cancellation exists only for scope deadline and explicit user
cancel — never for handler errors.

## State is a naming convention, not a concept

Reactions emit `state.updated{path, value}` as ordinary events in the
causal cone. The state projection is a trivial fold (apply deltas in
log order). Subscribing to state changes is subscribing to
`state.updated` (optionally filtered by path). The state schema is
visible *in the log*, and every update carries its causality — who
touched this path and why.

`state.updated` is **terminal**: it is a recorded fact, not a demand
for reaction. Terminality governs the orphan invariant, not routing —
subscribers to terminal events are fine (the printer already works
this way).

Open question deferred to the permission layer (Phase 4c scopes): who
is *allowed* to write a given `path`.

## Scopes: structured concurrency over a causal DAG

Classic structured concurrency (Trio nurseries, Kotlin
`coroutineScope`, Loom `StructuredTaskScope`) has four rules: tasks
spawn only inside a scope; a scope does not exit until all children
finish; a child's failure is the scope's failure; cancellation and
deadlines are scope properties. Its point is restoring *local
reasoning* about concurrent work.

reflex gets this in a different geometry: the scope is **causal, not
lexical**. The mapping:

| Structured concurrency | reflex |
|---|---|
| spawning a task | emitting a non-terminal event |
| the task tree | the causal DAG (`caused_by`) |
| scope awaits children | cone quiescence (Dijkstra–Scholten termination detection — the terminal-event invariant *is* this algorithm) |
| join / block exit | `scope.closed` from the progress projection |
| no orphans | the terminal-event invariant, verbatim |
| failure escalation | **deliberately rejected** — errors are events (above) |
| cancel/deadline flow down | dispatcher refuses delivery into cancelled/expired cones |

A scope is a *cut point* in the DAG — any event can root one. Nesting
falls out of the geometry. A drain is therefore **a view, not a
process**: a standing query declared in YAML —

```yaml
scopes:
  - root: RequestReceived     # which events open a scope
    closed_when: quiescent    # predicate over the progress projection
    deadline: 30s             # optional
```

— whose threshold crossings the bus announces as events
(`scope.opened`, `scope.closed`, `scope.deadline_reached`). Nothing is
stored; the scope is recomputed from the log.

The one concession to mechanism: the dispatcher itself reads the
progress projection at delivery time, so it can refuse events whose
cone contains a cancellation. The projection stays a view, but a view
the bus consults.

### Sub-scopes are the barrier primitive

A quiescence-triggered continuation must not subscribe to its *own*
scope's `scope.closed` — emitting new events into a cone just declared
quiescent self-invalidates the closure. Phases are sub-scopes: an
intent-classification phase roots a sub-scope at `intent.requested`;
its cone quiesces independently and earlier than the parent;
`scope.closed{root=intent.requested}` is an ordinary event in the
still-open parent cone, and the next phase subscribes to it.

The Phase 1.6 aggregator becomes a special case: "the fan-out's
sub-scope closed" replaces counting `EventDispatched.subscriber_count`.
One less concept.

## Deltas this implies in current code

1. `caused_by` becomes a list — join nodes (barriers, aggregators)
   have N causes; the causal structure is a DAG, not a tree. Cheap
   now, expensive after the Phase 4a wire format calcifies.
2. The bus stops treating handler errors as fatal: emit
   `{node}.failed` (non-terminal) and continue the drain.
3. Per-scope incremental quiescence detection — `DrainQuiesced` is
   currently computed once at full drain end; sub-scope barriers need
   it per cut point, online.
4. Generic projection — `SessionState`'s domain-shaped fields
   (`ToolCalls`, `ToolResults`) are pre-subject-era residue; the state
   projection folds `state.*`, the transcript fold is domain-blind.
5. Subject-style types throughout; `ToolCallProposed`/
   `ToolResultObserved` retire.
