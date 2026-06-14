# 00 — Reference: the agreed model at a glance

A cheat sheet for the decisions that are settled — event envelope, subject
taxonomy, the node vocabulary (each node by what it consumes and emits), and
naming. Each section is the *what*; the design docs are the *why*
([11](./11-domain-model.md) model, [12](./12-react-experiment.md) experiment,
[13](./13-event-taxonomy.md) taxonomy, [14](./14-target-coding-agent.md)
coding-agent target). Where a name is mid-migration the legacy form is noted as
`(was X)`.

> ⚠ **Partially superseded.** The consolidated settled-vs-open picture now
> lives in [24-concept.md](./24-concept.md); this page's node section
> predates the two-primitive reduction (see the warning below). Read 24
> first; use this page only for the envelope/subject tables, which stand.

Events do not float free: every kind is the **emit of some node and the consume
of another** — the graph is the subscription table. So the catalog is organised
by node, not as a flat list.

## Event envelope

Every event on the log has four parts, each with one home:

| Part | Shape | Carries |
|---|---|---|
| `subject` | `{class}.{scope...}.{kind...}` | routing + projection scope |
| `trace` | `{ session_id, request_id?, span_id, caused_by[], otel? }` | correlation, causation, telemetry context |
| `terminal` | `bool` | leaf of the causal DAG (no descendant expected) |
| `payload` | `{ ...data, source }` | data + origin metadata |

Replaces today's flat `Type` + `RequestID` + `Source` + scalar `CausedBy`.

### Trace ↔ OpenTelemetry

reflex concepts map onto OTel almost 1:1 — most fields are **derived**, not new:

| OTel | reflex | note |
|---|---|---|
| trace | **request** | one inbound → cone → answer = one trace; `trace_id` ≡ `request_id` |
| span | **event** | each reaction firing; `span_id` = the event id |
| `parent_span_id` | `caused_by[0]` | OTel spans have one parent… |
| span links | `caused_by[1:]` | …a join node's extra causes become links |
| `session.id` attribute | `session_id` | a session is many requests = many traces, grouped by attribute, **not** one trace |
| name / attributes / status | kind / payload / `*.failed`+terminal | |

- `session_id` is **new** — denormalised from the subject (dispatcher-stamped,
  like `request_id`), so OTel export needs no subject parsing. Absent on `sys.*`.
- `trace_id` / `span_id` / `parent_span_id` are a **projection**, not stored
  twice: `caused_by[]` stays the canonical DAG; the OTel parent/links view is
  derived at export.
- `otel?` is the one genuinely stored OTel field, and only for **distributed
  tracing**: an ingress carrying an upstream W3C `traceparent` is *adopted*
  (the resolver continues that trace) instead of minting a fresh one.
- **Metrics are projections, not fields.** `count(llm.completed)`,
  `histogram(latency)` are folds over the log; the event carries tracing
  context, never counters.

## Subjects

NATS grammar: `.`-delimited tokens, `*` = one token (any position), `>` = one+
tokens (tail only). Routing lives entirely in the subject.

```
sys.{kind...}                    machinery + global registry (session-less)
app.session.{id}.{kind...}       domain events, scoped by session
app.ingress.{surface}.{event}    inbound traffic before session resolution
```

- **Class first:** belongs to a session → `app.session.{id}.`; global machinery
  → `sys.`.
- **Session is the only subject scope.** Request membership is *not* a token —
  it lives in `trace.request_id`.
- **Handler desugar:** YAML `on: tool.fs.read.call` subscribes to
  `app.session.*.tool.fs.read.call`. Handlers speak kind; the bus adds scope.

### Scope is derived, not stored

`request_id` is stamped by the dispatcher as the **narrowest scope covering all
causes**: minted on `request.received`, inherited when all `caused_by` parents
agree, **empty** when they span requests (→ session-scoped). An event carries a
`request_id` ⇔ it is request-scoped. Recomputable from `caused_by`; never
hand-written.

## Nodes — consumes → emits

`type:` is the reaction archetype the YAML instantiates. `T` marks a terminal
emit. `{configured}` means the kind is set per-instance in YAML (`on:` / `emit:`),
not fixed by the type. The default vocabulary is small and **domain-blind**;
everything else is a tool plugin or `sys` machinery.

> ⚠ **Draft evolution:** [15-primitive-reduction.md](./15-primitive-reduction.md)
> proposes collapsing this eight-node vocabulary to **two primitives** (`llm` +
> `tool`) — `decode`/`signal`/`forward`/`router`/`aggregate`/`sink`/`tool_node`
> fold into direct subscription, a tool, or `sys` machinery. The engine side —
> what the runtime itself provides and guarantees (scopes, closure predicates,
> cancellation, budgets) — is specified in
> [16-engine-architecture.md](./16-engine-architecture.md). Read both before
> relying on the set below.

### User-declared (YAML)

| `type` | Role | Consumes | Emits |
|---|---|---|---|
| `llm` | reason | `llm.turn` | `llm.completed` · `llm.failed` |
| `decode` | translate | `llm.completed` | `tool.{name}.call` · `assistant.message` (T) · `request.handled` (T) · `llm.decode_failed` · `llm.emission_rejected` |
| `signal` | glue | `{configured X}` | `{configured Y}`, **empty** payload (the pump) |
| `forward` | glue | `{configured X}` | `{configured Y}`, **carrying** payload |
| `router` | route | `{configured X}` | one of `{Y₁…Yₙ}` by a predicate over payload/state |
| `aggregate` | join | `scope.closed` of a sub-scope | `{configured Y}`, folded from the sub-scope's cone |
| `sink` | surface | `{configured terminal kind}` | — (side effect to a surface: stdout, reply) |
| `tool_node` | act (peripheral) | `tool.{name}.call` | `tool.{name}.result` · `tool.{name}.failed` |

`assistant.message` is terminal only when it is the final answer; a mid-stream
message is non-terminal. `*.failed` emits are **non-terminal** (retry/fallback
is topology); `request.handled` / `assistant.message`(final) are **terminal**.

### Tool plugins (out-of-process)

Real tools are not builtin types — they are external binaries over SDK Remote
(`reflex daemon` + Unix socket), rooted to a workspace, under `plugins/` (doc
14). Each obeys the same contract as `tool_node`:

| Plugin | Consumes | Emits |
|---|---|---|
| `fs` | `tool.fs.read.call` · `tool.fs.edit.call` · `tool.fs.write.call` · `tool.fs.search.call` | matching `tool.fs.*.result` · `tool.fs.*.failed` |
| `fmt` | `tool.fmt.run.call` | `tool.fmt.run.result` · `tool.fmt.run.failed` |
| `lint` | `tool.lint.run.call` | `tool.lint.run.result` · `tool.lint.run.failed` |

The LLM tool menu is the projection of the consumers of `tool.*.call` — register
a plugin, the model gains the tool, zero config.

### `sys` machinery (runtime, not hand-wired)

The control-plane and lifecycle events all have a producer here — this is where
`sys.*`, `scope.*`, and the lifecycle terminals come from. Declared once by the
runtime, never wired per-graph.

| Node | Consumes | Emits |
|---|---|---|
| `session-resolver` | `app.ingress.*` | `request.received` (under resolved session) · `sys.state.updated` (new thread→session binding) |
| `scope-projection` | the progress projection over a cone | `scope.opened` · `scope.closed` `(was DrainQuiesced)` · `scope.deadline_reached` · `scope.budget_low` (F1) |
| `watcher` | post-quiescence of a scope | `event.orphaned` (T) `(was EventOrphaned)` · `request.unhandled` (T) |
| `audit` | `*` (or `sys.*`) | control-plane summaries / compression events |
| `clock` | runtime timer | `sys.clock.tick` |
| `dispatcher` (bus core) | `*` (it is the router) | `sys.handler.registered` / `.deregistered` · `sys.subscribed` / `.unsubscribed` · `sys.subscription.rejected` · `sys.event.dispatched` · `{node}.failed` (handler errored) · `loop.exhausted` (T) `(was LoopExhausted)` — and stamps `trace.request_id` |

### Where the request kinds come from

`request.received` is emitted by the `session-resolver`; `request.handled` by
`decode` (final action); `request.unhandled` by the `watcher`. So a request's
whole lifecycle is three different producers — none of it is special-cased
control flow, all of it is node emits.

### Retired node types

`echo` → split into `signal` / `forward` (F4: signals must not smuggle
payload). `terminator` → dropped (terminality is a flag any reaction sets).
`tool_call` → dropped (payload routing). Retired domain-specific handlers →
not defaults (fold into `router` or become plugins).
`ToolCallProposed` / `ToolResultObserved` kinds retire into subject-typed
`tool.*`.

## Naming conventions

- Subjects are `lower.dotted`; the kind suffix reads as a fact (`*.completed`,
  `*.failed`, `*.received`).
- **Scope** goes in the subject, **correlation** (`request_id`) in the trace,
  **origin** (`source`) in the payload — one axis, one home.
- Errors are `*.failed`, **non-terminal**. Recorded facts (`state.updated`,
  `request.handled`) are **terminal**.
- A tool's three kinds are always `tool.{name}.call|result|failed`.
