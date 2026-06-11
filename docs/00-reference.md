# 00 — Reference: the agreed model at a glance

A cheat sheet for the decisions that are settled — event envelope, subject
taxonomy, naming, the event catalog, and the builtin node types. Each section
is the *what*; the design docs are the *why* ([11](./11-domain-model.md) model,
[12](./12-react-experiment.md) experiment, [13](./13-event-taxonomy.md)
taxonomy, [14](./14-target-coding-agent.md) coding-agent target). Where a name
is mid-migration the legacy form is noted as `(was X)`.

## Event envelope

Every event on the log has four parts, each with one home:

| Part | Shape | Carries |
|---|---|---|
| `subject` | `{class}.{scope...}.{kind...}` | routing + projection scope |
| `trace` | `{ request_id?, caused_by[] }` | correlation + causation |
| `terminal` | `bool` | leaf of the causal DAG (no descendant expected) |
| `payload` | `{ ...data, source }` | data + origin metadata |

Replaces today's flat `Type` + `RequestID` + `Source` + scalar `CausedBy`.

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

## Event catalog

Kind suffixes only (scope prefix omitted). `T` = terminal.

### Lifecycle

| Kind | T | Meaning |
|---|---|---|
| `request.received` | | a resolved request enters its session scope `(was RequestReceived)` |
| `request.handled` | T | the request produced its answer `(was RequestHandled)` |
| `request.unhandled` | T | quiesced with no answer — visibly, not silently `(was RequestUnhandled)` |

### Reasoning

| Kind | T | Meaning |
|---|---|---|
| `llm.turn` | | signal: take the next reasoning step (empty payload) |
| `llm.completed` | | the raw model completion |
| `llm.failed` | | model/transport error (non-terminal) |
| `llm.decode_failed` | | completion did not parse into an action |
| `llm.emission_rejected` | | decoded action not in the allowlist |

### Tools (per tool `{name}`)

| Kind | T | Meaning |
|---|---|---|
| `tool.{name}.call` | | invoke the tool with structured args |
| `tool.{name}.result` | | success payload |
| `tool.{name}.failed` | | tool error (non-terminal — retry/fallback is topology) |

### Conversation & state

| Kind | T | Meaning |
|---|---|---|
| `assistant.message` | T* | a message to the user (terminal when final) `(was AssistantMessageProposed)` |
| `state.updated` | T | a `{path, value}` delta; the state projection folds these |

### Progress & scope

| Kind | T | Meaning |
|---|---|---|
| `scope.opened` | | a cut point opened a scope |
| `scope.closed` | | the scope's cone quiesced `(was DrainQuiesced)` |
| `scope.deadline_reached` | | scope deadline hit |
| `scope.budget_low` | | one step before a loop cap — lets the model wrap up (F1) |
| `loop.exhausted` | T | a capped cycle hit its limit `(was LoopExhausted)` |
| `event.orphaned` | T | a non-terminal event reached quiescence with no reaction `(was EventOrphaned)` |

### Control plane (`sys.`)

| Kind | Meaning |
|---|---|
| `sys.handler.registered` / `.deregistered` | a handler joined/left `(was HandlerRegistered/...)` |
| `sys.subscribed` / `.unsubscribed` | a subscription opened/closed `(was Subscribed/...)` |
| `sys.subscription.rejected` | a subscription denied by permission |
| `sys.state.updated` | the global session registry (thread→session bindings) |
| `sys.clock.tick` | runtime-injected time |

**Retired:** `ToolCallProposed`, `ToolResultObserved` (payload-routed RPC →
subject-typed `tool.*`). Domain-example kinds (`TriageDecided`, `GhQueryResult`,
`TargetParsed`, `ClassificationsAggregated`, …) are *not* core — they belong to
their example graphs.

## Builtin node types

`type:` is the reaction archetype the YAML instantiates. The default vocabulary
is small and **domain-blind**; everything else is a plugin or `sys` machinery.

### User-declared (YAML)

| `type` | Role | Consumes → Emits |
|---|---|---|
| `llm` | reason | `llm.turn` → `llm.completed` (backend pluggable: gemini/stub/…) |
| `decode` | translate | `llm.completed` → `tool.*.call` \| `assistant.message` \| `request.handled` (action allowlist) |
| `signal` | glue | X → Y, **empty** payload (the pump) |
| `forward` | glue | X → Y, **carrying** payload |
| `router` | route | X → one of N kinds by a predicate over payload/state |
| `aggregate` | join | `scope.closed` of a sub-scope → one folded event |
| `sink` | surface | a terminal kind → an external surface (stdout, reply) |
| `tool_node` | act (peripheral) | `tool.{n}.call` → `.result`/`.failed` for trivial **in-bus** pure fns |

### Runtime machinery (not hand-wired)

`session-resolver` (`app.ingress.* → request.received`, reads the `sys` registry),
the scope/budget/orphan projections (emit `scope.*`, `event.orphaned`,
`request.unhandled`), `audit`, and the dispatcher itself. Declared once by the
runtime, emitted as `sys.*` / scope events.

### Tools are plugins

Real tools (`fs`, `fmt`, `lint`, …) are **out-of-process plugins** over SDK
Remote (`reflex daemon` + Unix socket), rooted to a workspace, under `plugins/`
— not builtin types. See [14](./14-target-coding-agent.md).

### Retired node types

`echo` → split into `signal` / `forward` (F4: signals must not smuggle
payload). `terminator` → dropped (terminality is a flag any reaction sets).
`tool_call` → dropped (payload routing). `parse_target` / `triage_rules` /
`gh_query` → not defaults (fold into `router` or become plugins).

## Naming conventions

- Subjects are `lower.dotted`; the kind suffix reads as a fact (`*.completed`,
  `*.failed`, `*.received`).
- **Scope** goes in the subject, **correlation** (`request_id`) in the trace,
  **origin** (`source`) in the payload — one axis, one home.
- Errors are `*.failed`, **non-terminal**. Recorded facts (`state.updated`,
  `request.handled`) are **terminal**.
- A tool's three kinds are always `tool.{name}.call|result|failed`.
