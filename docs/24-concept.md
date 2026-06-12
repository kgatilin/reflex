# 24 — Concept: the settled model and the open questions

> **Status: CONSOLIDATION / normative index.** One document collecting
> everything that is *decided* across docs 00–23 and everything that is
> still *open*. The per-topic documents remain the "why" — each section
> here cites its sources; this is the canonical "what". Where an older
> document disagrees with this one, this one reflects the later
> convergence: docs 00–10 describe the legacy (shipped) era and are
> superseded in vocabulary and several mechanisms by the converged model
> of docs 11–21 (the supersession table closes Part I). Docs 22–23 are
> the bootstrap plan and its live operational companion.

---

## Part I — Settled

### 1. Three concepts, one invariant

([11](./11-domain-model.md))

```
Event      a record on the append-only log: subject, trace, terminal flag, payload
Reaction   a pure function (event, views) → events; llm, tools, projectors —
           all Reactions, no exceptions
Projection a declared fold(log) → view; structural threshold crossings are
           announced back onto the log as events
```

**The invariant, never violated: anything that cannot be recomputed from
the log is a bug.** Session state, scope status, subscription tables,
budgets — all views, never stores.

There is no fourth concept. Everything else is a convention over these
three: errors are `*.failed` events (non-terminal); state writes are
`state.updated.{path}` events (terminal); retry/fallback is topology;
barriers are subscriptions to `scope.*.closed`; time is `clock.tick`
events; the LLM is a reaction filling graph gaps.

### 2. The envelope and the subject grammar

([13](./13-event-taxonomy.md), refined by [18](./18-pipeline-walkthrough.md))

Every event has four parts, one home per axis — scope in the subject,
correlation in the trace, origin in the payload:

| Part | Shape |
|---|---|
| `subject` | `{class}.{scope...}.{kind...}` — NATS grammar (`*` one token, `>` tail) |
| `trace` | `{ session_id, request_id?, span_id, caused_by[], otel? }` |
| `terminal` | bool — leaf of the causal DAG |
| `payload` | `{ ...data, source }` |

Settled rules:

- **Classes**: `sys.{kind}` (session-less machinery), `app.session.{id}.{kind}`
  (domain), `app.ingress.{surface}.{event}` (pre-resolution inbound).
- **Session is the only subject scope.** Request membership rides in
  `trace.request_id`, stamped by the dispatcher as the narrowest scope
  covering all causes — minted on `request.received`, inherited when all
  parents agree, empty when they span requests. Recomputable from
  `caused_by`; never hand-written.
- **`caused_by[]` is a list** (join nodes have N causes) and is
  **engine-stamped, never handler-chosen** — the uprightness rule of
  [17](./17-quiescence-prior-art.md). Membership in a scope is a fact of
  the log, not a sender decision.
- **Handler desugar**: handlers bind kind, the bus prepends the scope
  wildcard; projections do the opposite. With scope-qualified
  subscriptions ([16](./16-engine-architecture.md)) the grammar is the
  full lattice {kind pattern} × {scope pattern}.
- **State paths live in the subject** (`state.updated.intent`,
  `state.updated.memory.note`), so state subscription is a wildcard, not
  a payload filter. Named scopes get **typed closure kinds**
  (`scope.intent.closed`, `scope.brain.closed`) — phase wiring is
  type-level. Both from [18](./18-pipeline-walkthrough.md).
- **OTel maps almost 1:1, derived not stored**: request ≡ trace, event ≡
  span, `caused_by[0]` ≡ parent, `caused_by[1:]` ≡ links, session is an
  attribute. `otel?` is stored only to adopt an upstream `traceparent`.
  Metrics are folds over the log, never event fields.
- **Session resolution**: an adapter emits a session-less ingress event;
  a resolver maps it to a session via the `sys.state.updated.session.binding.*`
  registry (itself a kv fold) and emits the scoped `request.received`.

### 3. Two primitives

([15](./15-primitive-reduction.md))

The user vocabulary is two reaction bodies:

| `type` | Role | Contract |
|---|---|---|
| `llm` | reason | folds its declared views, calls the model, emits typed actions from an allowlist |
| `tool` | act | consumes `tool.{name}.call`, emits `.result` / `.failed`; out-of-process plugin or in-bus pure fn — one concept, deployment detail |

Everything else **folded away**: `decode` into `llm` config;
`seed`/`pump`/`signal`/`forward`/`llm.turn` into direct subscription;
`router` into subscription (type fork) or a tool (data fork);
`aggregate` into "subscribe to `scope.closed`"; `sink` into a
fire-and-forget tool; `terminator`/`tool_call`/`relay` dropped.

Two structural consequences, both settled:

- **Fan-out synchronization is a projection, not a node.** A firing that
  emits tool calls roots a scope; the cone quiesces when all results are
  in; `scope.{node}.closed` is the barrier. **N=1 is the degenerate
  fan-out** — sequential and parallel are one mechanism. The loop is a
  chain of scope instances; "turn" is not a vocabulary word.
- **The LLM tool menu is a projection of the `tool.*.call` consumers.**
  Register a plugin, the model gains the tool; remove the subscription,
  the menu updates on the next firing. Zero config.

### 4. The engine: two mechanisms

([16](./16-engine-architecture.md), [17](./17-quiescence-prior-art.md))

```
append     put an event on the log (the sole write)
dispatch   fan the event out to the live table — stamping correlation
           (request_id, session_id, caused_by — uprightness), consulting
           the progress projection before delivery, enforcing budgets
```

- A handler error during dispatch is **not fatal**: the engine appends
  `{node}.failed` (non-terminal) into the cone and continues. Nothing
  unwinds.
- **The engine never branches on payloads.** Dispatch, closure,
  stamping read structure only — subjects, terminality, `caused_by`.
  Materializing declared views ([§6](#6-projections-declared-folds-over-causal-horizons))
  is blind copying, not a decision.
- The richness of the system does not live in the node vocabulary; it
  lives in the **progress projection over the `caused_by` DAG** — where
  loops, barriers, budgets, cancellation, and orphan detection all come
  from. That machinery is §5.

**The guarantees** (each load-bearing):

- **G1** recompute-from-log — every engine structure is a rebuildable fold.
- **G2** no blocking — waiting is a subscription, nothing parks.
- **G3** errors are events — non-terminal `{node}.failed`, drain continues, no unwinding.
- **G4** no silent dead ends — `event.orphaned` / `request.unhandled` announce every dangle.
- **G5** crash recovery ≡ retry — recompute frontier, re-dispatch; at-least-once; effectful tools carry idempotency keys with intent-before-effect (log as outbox).
- **G6** the barrier fires exactly once per scope, `caused_by` = the cone's frontier.
- **G7** no unbounded work — every cone under a budget.
- **G8** no privileged plane — engine decisions are ordinary events on the same log.
- **G9** cancellation is confined and recorded.

**The anti-catalog** (deliberate absences): no retry mechanism, no error
escalation, no state store, no orchestrator or agent state, no payload
inspection in dispatch, no synchronous request/reply.

### 5. Scopes and the progress projection

([15](./15-primitive-reduction.md) fan-out, [16](./16-engine-architecture.md)
geometry and algebra, [17](./17-quiescence-prior-art.md) mechanics,
[18](./18-pipeline-walkthrough.md) worked end-to-end,
[20](./20-topology-management.md) interventions)

The engine's central mechanism. Loops, barriers, phases, subagent
isolation, budgets, cancellation, and the request lifecycle are all one
concept: a **scope** — a cut point in the causal DAG, a root event plus
the cone it dominates.

**Membership is geometry, never addressing:**

```
E ∈ scope(R)  ⟺  R is an ancestor of E
               and the path R → E does not pass through scope.closed(R)
               and no other scope root lies on the path R → E
```

`caused_by` is engine-stamped, so events *land* in scopes by causality
alone — no handler can place an event in a scope of its choosing. The
three clauses are three load-bearing rules:

- **Sealing** (second clause): `scope.closed` is the boundary through
  which causality *exits* the scope, so a closure's consumer lives in
  the parent scope *by construction* — "the next firing never emits back
  into the just-closed cone" is geometry, not discipline. Exactly
  structured concurrency: a join's continuation belongs to the enclosing
  block.
- **Partition** (third clause): an event belongs to its nearest
  enclosing scope. Nesting is transitive quiescence for free — a child's
  closure is itself a non-terminal event in the parent's cone, so the
  parent cannot close before every child has. No nesting mechanism
  exists, only the membership rule.
- **Blast radius**: cancellation reaches exactly the cancelled cone
  (G9), because the membership rule *is* the confinement rule.

**Rooting has exactly two sources — there is no fan-out heuristic:**

- **Declared**: a `scopes:` entry roots on a kind
  (`root: request.received`) and carries the configuration — budget,
  deadline, `closed_when`. Phases live here.
- **Node-rooted**: a node firing that emits *work* (≥1 tool call) roots
  an instance; emitting a message roots nothing — a message is a fact,
  not a demand. Not a second concept: the same named scope with a
  different root specifier.

Every scope is **named** (a single subject token; node scopes default to
the node name) and closures are **typed kinds** — `scope.brain.closed`,
`scope.intent.closed`; `scope.*.closed` is the audit wildcard. Instances
are distinguished causally (by root event), never by name. "Turn" is not
a vocabulary word: the log only ever shows firings rooting scopes and
scopes closing.

**Quiescence is obligation counting** (Naiad's occurrence counts,
recast):

```
dispatch of E to N subscribers   → +N on every scope root up E's ancestor chain
a handler completes              → its emits' own increments, then −1
obligations(R) == 0              → scope.closed{R}, exactly once (G6)
```

- An **orphan is the zero-crossing** of an event's own counter — orphan
  detection and quiescence are one mechanism observed at two
  granularities, not a post-hoc DAG sweep.
- A pending **plugin call holds its obligation open** until the result
  event is *appended* — an out-of-process tool holds its cone open;
  without this the barrier fires early.
- Cost: O(nesting depth) counter updates per event, no DAG traversal on
  the hot path. The **single-writer log makes the counts exact** — it
  buys back the ~80% of Naiad's protocol (the distributed exchange) that
  needed mechanized verification.

**Closure is a predicate — the algebra is the synchronization
vocabulary** (Go's `sync` package on the bus, none of it a node type):

| Idiom | `closed_when` | on close |
|---|---|---|
| WaitGroup (default) | `quiescent` | — |
| errgroup (opt-in, never implicit) | `quiescent OR any *.failed` | cancel the rest |
| race | `any child terminal` | cancel the rest |
| quorum | `count(child terminal) ≥ N` | cancel the rest |

`quiescent` is the failure-tolerant default: a `tool.b.failed` does not
stall or abort the barrier — the cone quiesces, the failure sits in the
closed fold, the consumer sees it and decides. Any predicate that can
close *before* quiescence **must** cancel the remaining cone; early
closure without cancellation is inexpressible. Predicates read structure
(kinds, terminality, counts), never payload values — a payload condition
is a reaction's job. `scope.closed.caused_by[]` is the cone's frontier:
the causal join (one OTel span, N links), which is why nothing upstream
of a barrier is ever orphaned.

**Budgets — and loops are budgets.** Every quantitative limit is one
triple: a counting fold per kind (plus wall clock) → a soft
`scope.budget_low` warning into the cone one step early (the model can
wrap up with what it has) → the `scope.budget_exhausted` hard backstop
(refused dispatch). `loop: max_iterations` dissolves: in cone geometry
each iteration is fresh DAG nodes, so "bound the loop" *means* "bound a
kind's count within a cone". A **mandatory default budget on the session
scope** makes all work bounded (G7); the Tarjan cycle check survives as
a lint ("this cycle is not covered by a tight budget").

**Cancellation and deadlines.** Exactly two sources — a deadline
elapsing and an explicit cancel; handler errors never cancel.
Propagation is refused dispatch read off the cone, never interruption:
an in-flight tool finishes, its result is appended (the log records what
happened) but not dispatched into the cancelled cone.

**Declarations pin, interventions target** ([20](./20-topology-management.md)):
a declaration change governs *new* instances — an instance reads its
configuration at its root's log position, replay-stable. Touching a
*live* instance is an explicit, audited event into its cone:
`sys.scope.{instance}.budget.extended` / `.deadline.extended` /
`.cancelled`.

**Scope-qualified subscriptions** complete the grammar lattice:
`on: tool.*.failed, in: research` delivers only failures inside the
«research» scope — a delivery-time filter on the ancestor-scope walk the
dispatcher already performs. Rooting and listening are configurable;
**membership is not**.

**What this one mechanism carries across the docs:**

- the agent loop — a chain of node-scope instances S₁→S₂→…
  ([15](./15-primitive-reduction.md)); sequential and parallel tool
  calls are one path (N=1 is the degenerate fan-out);
- the phase barrier — subscribe `scope.intent.closed`; zero enrichers ⇒
  zero obligations ⇒ instant closure, no special case
  ([18](./18-pipeline-walkthrough.md));
- elastic barriers — repair/medic lanes inserted between call and result
  stretch the brain's scope automatically; a counting aggregator breaks,
  quiescence does not ([18](./18-pipeline-walkthrough.md));
- handoff and judging — anyone subscribes a closure; a judge per brain
  step or a synthesizer taking over is one `on:` line
  ([16](./16-engine-architecture.md)/[21](./21-operator-exercise.md));
- subagent context isolation — a transcript view bound `in:` the
  subagent's scope sees only its cone; fresh-context delegation is one
  line ([19](./19-projections.md)/[21](./21-operator-exercise.md));
- human-in-the-loop — the cone always closes promptly; waiting is
  session state, never an open scope
  ([18](./18-pipeline-walkthrough.md));
- the request lifecycle — `scope.request.closed` ends the OTel trace and
  is where the audit fold runs.

### 6. Projections: declared folds over causal horizons

([19](./19-projections.md))

```yaml
projections:
  - name: fs.seen
    on:    [tool.fs.read.result, tool.fs.edit.result, tool.fs.write.result]
    in:    session          # horizon — where the backward walk stops
    shape: kv               # kv | log — deliberately final
    key:   payload.path
    value: payload.sha
```

- The reference evaluation is a **backward walk over `caused_by`** from
  the read position to the horizon root. Caches and snapshots are
  evaluation strategies; the walk is the definition (G1 by construction).
- **Two shapes only.** Anything richer is a reaction: subscribe,
  compute, emit `state.updated.{path}`; the generic kv over
  `state.updated.>` serves the result. kv ties between incomparable
  branches break by log order (single writer ⇒ deterministic).
- **A read is anchored at the trigger.** "The model had it in context"
  and "it is in the call's causal past" are the same predicate — the
  geometry the read-before-edit guard rides on. Parallel branches are
  isolated for free.
- **Access is `reads:` on the subscription, attached at dispatch**:
  `React(event, views) → events`. Raw log access retires; the llm's
  transcript is itself a declared `log` projection. No query RPC —
  replay reproduces every read exactly.
- **Registration rides the control plane** (`sys.projection.registered`),
  YAML block or plugin `hello` — a plugin brings its tools *and* its
  projections in one changeset.
- **Sessions are causal chains** — the one genuinely new rule: the
  resolver chains each `request.received` to the previous request's
  closure, so a session is one connected cone and `in: session`
  horizons just walk it.
- **The engine never branches on payloads** — it *evaluates* declared
  selectors (blind copying) but no dispatch/closure/stamping decision
  reads a domain view.

### 7. Topology management: changesets

([20](./20-topology-management.md); kept from [05](./05-control-plane-as-events.md)/[06](./06-permissions-and-scopes.md))

```
sys.topology.changeset.requested{ ops, principal }
   → engine validates fold(live table) + ops → resulting graph
   → facts (sys.node.registered, sys.subscribed, sys.scope.declared, …)
     + changeset.applied | changeset.rejected{ reasons }
```

- **Only the engine writes facts** — the control plane's uprightness
  rule. No client can claim a subscription into existence.
- **The resulting graph is validated, not each step**; a changeset
  applies atomically between dispatches; intermediate states are
  inexpressible. In-flight work drains naturally (obligation counting).
- **Four managed object kinds**: nodes, subscriptions, **scopes**, and
  **projections** — budgets/deadlines/predicates are runtime-editable
  topology, not config-file edits.
- **Two severities**: *reject* (structurally invalid graph) vs *lint*
  (`sys.lint.*` events — legal but suspect, e.g. a cycle without a
  tight budget). Lints are subscribable; escalation policy is topology.
- **Declarations pin at the root; interventions are events**: a config
  change governs new instances; "extend this stuck request's deadline"
  is `sys.scope.{instance}.budget.extended` / `.deadline.extended` /
  `.cancelled` — audited, causally placed.
- **One pipeline for every source**: boot, admin CLI, optimiser, hot
  reload, plugin `hello`. `reflex validate` is a dry-run of the same
  validator. History/diff/rollback come free from the log.
- **Kept from docs 05/06**: the live table is a fold of control-plane
  events; audit is an ordinary subscriber; the permission grammar
  (scope patterns, conservative wildcards, reserved default-deny zones,
  recursive `meta.grant` rooted in boot) — the Phase 4c synchronous
  APIs become sugar over the changeset pair, checked on the `requested`
  event's principal.

### 8. Standing conventions and patterns

All settled in design sessions; sources cited:

- **Errors are non-terminal events; recovery is topology** ([11](./11-domain-model.md),
  validated live in [12](./12-react-experiment.md) W2). Failure
  escalation is rejected; errgroup semantics is opt-in per scope.
- **Human-in-the-loop is a typed result, never a suspension**
  ([18](./18-pipeline-walkthrough.md)): `tool.transfer.confirm_required`
  closes the cone; the pending action is session state; the human's
  reply is a new request chained through the session. Nothing ever
  waits. Corollary: a request can end *answered but unfinished* —
  `request.handled` is distinct from "everything done".
- **Repair/error lanes** ([18](./18-pipeline-walkthrough.md)):
  draft → args-repair → call, medic on `tool.*.failed`. The causal
  barrier is insensitive to chain length — lanes insert and remove
  without touching synchronization.
- **Subagent isolation is a horizon** ([21](./21-operator-exercise.md)):
  a sub-llm whose transcript view is bound `in:` its own scope gets
  fresh-context delegation in one line.
- **Verification is enforceable topology** ([21](./21-operator-exercise.md)):
  the gate pattern (brain emits `draft.message`; a deterministic gate
  folds the cone and passes/returns) makes "answering unverified"
  inexpressible rather than discouraged.
- **Crystallization** ([08](./08-optimization-as-rewrite.md),
  [18](./18-pipeline-walkthrough.md), [21](./21-operator-exercise.md)):
  LLM-route first; when the corpus shows regular behaviour at a gap,
  compile it into a deterministic node by changeset. The optimiser is a
  changeset client bounded by scope grants; behavioural equivalence on
  the trace corpus is the acceptance criterion.
- **Streaming is a non-gap** ([21](./21-operator-exercise.md) #5): a
  transport concern of the reply sink; the log records outcomes, not
  keystrokes.
- **Cassette replay is a testing tool, not a correctness requirement**
  ([12](./12-react-experiment.md) F3): regression runs and optimiser
  equivalence checks fold recorded `llm.completed` facts.
- **Terminal ≠ no reaction** ([18](./18-pipeline-walkthrough.md)):
  terminality governs orphan accounting only; dispatching a terminal
  event still opens obligations.

### 9. The coding agent and the bootstrap

([14](./14-target-coding-agent.md), [22](./22-bootstrap-self-hosting.md), [23](./23-bootstrap-roadmap.md))

- **Tools are out-of-process plugins** over SDK Remote, structured
  per-tool JSON payloads, one subject triple `tool.{name}.call|result|failed`.
- **Confinement is plugin rooting** (`--root` clamp), not a bus gate —
  the guard reads payloads, so it lives in domain code (engine
  domain-blindness demands it).
- **Read-before-edit is the `fs.seen` projection + a disk compare** —
  no `base_sha` threading; "no prior read" and "stale read" are the two
  failures. The audit fold is the same projection evaluated offline.
- **No bash, definitively** — deterministic single-purpose tools only
  (`fs.*`, `fmt`, `lint`, `go.build/test/vet`); a safety boundary for a
  self-hosting agent, not a style choice. Search is in-process (no
  smuggled process spawn).
- **No self-wiring**: the agent writes code; only the operator wires
  topology. Self-improvement crosses a human boundary twice — reviewed
  commits and operator changesets.
- **Multi-model is an `llm`-body concern**: one provider interface,
  three adapters behind Vertex AI (Gemini / Anthropic / OpenAI-compatible),
  canonical events, model binding per node as config. Model–role fit is
  a placement decision (strong brain; cheap repair lanes; thinking
  models in no-tool seats).
- **The bootstrap frame**: stage 0 (hand-built kernel on the legacy
  loop) is the last hand-written code; the agent implements docs 16–20
  as tasks; each landed feature's adoption by the agent's own topology
  is its acceptance test. Cost is measured per task (`llm.usage` →
  `reflex costs`). Operating loop and task queue live in
  [23](./23-bootstrap-roadmap.md).

### 10. Implementation status and supersession

Three strata of truth:

1. **Shipped, legacy vocabulary** (phases 1–4d, docs 00–10): the
   in-memory bus and drain, daemon + SDK Remote, control-plane events +
   live table, permission layer, analyzer/objective, loop caps,
   aggregator, projection store, wait predicates.
2. **Shipped, stage 0** ([23](./23-bootstrap-roadmap.md)): provider
   interface + Vertex-Anthropic adapter, multi-model `llm` body with
   native tool calls and `llm.usage`, `fs` and `gotool` plugins, cost
   tracking, the crutch topology `examples/agent.yaml`.
3. **Designed, not built** (docs 13–20): the subject/trace envelope,
   obligation counting and per-scope quiescence, node-rooted scopes and
   the closure algebra, cancellation/deadlines, budgets, the projection
   walk + `reads:`, changesets. These are the bootstrap task queue.

What the converged model **retires** from the shipped code (replacement
in parentheses — each retirement is a bootstrap task, not a drift):

| Legacy (shipped) | Superseded by |
|---|---|
| Eight-node vocabulary (`decode`, `signal`, `forward`, `router`, `aggregate`, `sink`, …) | two primitives + topology ([15](./15-primitive-reduction.md)) |
| PascalCase `Type` + flat `RequestID`/`Source`, scalar `CausedBy` | subject + trace envelope, `caused_by[]` ([13](./13-event-taxonomy.md)) |
| `projection.Store` / `ProjectionAware` / `proj_get`/`proj_set` / `--wait projection.has` | declared folds + `reads:` attach ([19](./19-projections.md)) |
| Aggregator counting `EventDispatched.subscriber_count` | subscribe to `scope.*.closed` ([15](./15-primitive-reduction.md)/[16](./16-engine-architecture.md)) |
| `loop: max_iterations` + `LoopExhausted` | per-kind scope budgets + `scope.budget_exhausted` ([16](./16-engine-architecture.md)) |
| Fatal handler errors aborting the drain | non-terminal `{node}.failed`, drain continues ([11](./11-domain-model.md)) |
| `SubscribeAs`/`UnsubscribeAs` synchronous mutation APIs | sugar over `changeset.requested` ([20](./20-topology-management.md)) |
| Per-operation subscription validation | resulting-graph changeset validation ([20](./20-topology-management.md)) |
| `DrainQuiesced` once per full drain | per-scope online closure ([17](./17-quiescence-prior-art.md)) |
| `seed`/pump topology of `examples/react.yaml` and `agent.yaml` | direct subscription + node-rooted scopes (bootstrap iteration 2) |

What **survives unchanged** from the legacy era: events-only and no
privileged plane ([01](./01-mental-model.md)); the live table as a fold;
audit as an ordinary handler; the permission grammar ([06](./06-permissions-and-scopes.md));
the daemon/SDK transport; Tarjan (as a lint); the terminal-event
invariant (as the log-level view of quiescence).

---

## Part II — Open

### A. Design gaps that need a document (ranked, from [21](./21-operator-exercise.md))

1. **Context budget and view compaction — the largest hole, the named
   next design target.** Engine budgets count events; agents die of
   tokens. The `log` shape has no window and no compaction story; the
   unresolved core is a **compaction event as a horizon cut** ("fold
   from the last checkpoint") — nothing in the declaration grammar can
   say it today. Also doc 23's cost lever #1 in disguise: input tokens
   grow quadratically with task length until this lands.
2. **Log payload weight.** `tool.fs.read.result` carries file content;
   the append-only log becomes a blob store. Accept explicitly, or
   define a sha-keyed sidecar that G1 must then declare part of the
   log. Must be *decided*, not drifted into. (Same hole as #1 at a
   different altitude: the fold must fit.)
3. **Mid-flight steering — the rule is unwritten.** Machinery exists:
   steering = `sys.scope.{instance}.cancelled` + the resolver chaining
   the new request through the cancelled closure, so all partial
   progress is in the new fold. Needs one written section (likely in
   doc 19's session-chain rule).
4. **Node config: `sys.node.updated` vs config-as-facts.** The original
   gap ([21](./21-operator-exercise.md) #4): a prompt change is
   deregister + register; prompt engineering through changesets seemed
   to need a first-class update kind. The leading resolution (design
   session) **dissolves the kind instead of adding it** — split a node
   into *wiring* and *behaviour*:
   - **Wiring** (`on` / `emit` / `reads`) stays a changeset-validated
     fact (`sys.node.registered`) — it is what the static graph and the
     validator read.
   - **Behaviour** (prompt, model binding, params, action allowlist)
     becomes ordinary log facts — `sys.state.updated.node.brain.config{…}`,
     one whole-object value so a swap is one atomic event — and the
     body `reads:` them as a kv view. The `llm` body becomes **one
     universal function**, parameterized entirely by the log; "the
     brain" and "the medic" differ only in subscriptions plus config
     facts.

   What this buys: prompt/model versioning for free with "which change
   moved the metric" attached (the original ask); A/B = emit one event;
   doc-06 permissions already answer "who may write the path"; the
   standing test passes — zero new primitives, a convention over
   state + projection. What it costs, each acceptable and recorded:
   (a) the static graph keeps only the declared `emit:` upper bound —
   "allowlist ⊆ emit" becomes a lint-reaction on config events, not a
   wiring-time check (same fate as doc 22's R1-seat lint); (b) sys-level
   facts are causally disconnected from running cones, so the config
   view needs a **log-order global horizon** the doc-19 grammar lacks
   (`in:` is request|session today) — precedent already exists: the LLM
   tool menu is a kv fold over `sys.subscribed`, equally outside any
   cone; (c) read-at-trigger means later firings of an open request see
   a mid-request config change — principled, not accidental:
   *cumulative* semantics (budgets, counters) pin at the instance root
   (doc 20), *memoryless* semantics (prompt, model) read at the
   trigger's log position, both replay-stable. Status: lean recorded,
   to ratify with a worked doc or a bootstrap iteration.
5. **The operator's eval loop** needs "fork a log at a position under a
   different topology" — replay (G5) + cassettes nearly suffice; the
   fork operation is missing. Tooling over the model, but without it
   the improvement loop has no regression harness.
6. **The trace workbench** ([21](./21-operator-exercise.md) operator
   loop): queries over folds ("every trace where an edit was not
   followed by a lint"). Doc 04 grown up; without it the operator loop
   is blind.

### B. Recorded leans — decisions taken provisionally, to ratify or refute

| Question | Source | Lean |
|---|---|---|
| Refused-delivery representation: per-refusal event vs derivable | [16](./16-engine-architecture.md) | derivable (less noise); audit must compute it |
| Quorum/race cancellation: wasted in-flight work at high fan-out | [16](./16-engine-architecture.md) | accept (log records results); cost model unexamined |
| Keep raw `llm.completed` as terminal record for replay | [15](./15-primitive-reduction.md) | keep (stage 0 emits it) |
| `relay` (pure rename node) resurrection | [15](./15-primitive-reduction.md) | dropped until a real seam demands it |
| `edit{old,new}` vs whole-file `write` for weak models | [14](./14-target-coding-agent.md) | edit primary; decide on live-run evidence |
| One multi-tool plugin binary vs one per family | [14](./14-target-coding-agent.md) | per family (stage 0 shipped `fs`, `gotool`); reversible |
| Projection evaluation: lazy walk vs cached snapshots crossover | [19](./19-projections.md) | implementation-only; contract unaffected |
| Wire weight of `log` views per delivery | [19](./19-projections.md) | delta-encode later; semantics unchanged |
| kv tie-break visibility (flag a tie?) | [19](./19-projections.md) | silent; audit fold can detect |
| Empty-horizon reads: error or empty view | [19](./19-projections.md) | wiring-time lint, runtime empty view |
| Changeset concurrency | [20](./20-topology-management.md) | validate at append under the single writer; optimistic, honest `rejected{conflict}` |
| Plugin `hello` partial apply | [20](./20-topology-management.md) | reject whole; plugin retries with a fixed manifest |
| Rollback fidelity (table restored, instances pinned) | [20](./20-topology-management.md) | accept; document the pinning consequence |
| Lint-escalation policy as a pre-apply validator | [20](./20-topology-management.md) | built-in severities only until a deployment demands more |
| Session minting: adapter-supplied key vs resolver auto-mint | [13](./13-event-taxonomy.md) | open — per-surface (Slack thread auto-mints; alert/cron groups) |
| Who may write a `state.updated.{path}` | [11](./11-domain-model.md)/[19](./19-projections.md) | hook exists (emits/reads checks at declaration level); grammar unwritten |
| When the agent gets `git.commit` | [22](./22-bootstrap-self-hosting.md) | after the verification gate + confirm-gated flow; never ungated |
| When the agent gets changeset rights | [22](./22-bootstrap-self-hosting.md) | never directly; proposed-changeset + operator approval |
| Prompt caching in the anthropic adapter | [22](./22-bootstrap-self-hosting.md)/[23](./23-bootstrap-roadmap.md) | deferred until input tokens dominate the cost log; neutral log does not obstruct it |
| Node behavioural config as log facts + one universal `llm` body | Part II A.4 | adopt with the wiring/behaviour split; ratify via a worked doc or bootstrap iteration |
| Global (log-order) horizon for views over `sys.*` facts | [19](./19-projections.md) + A.4 | add `in: global` to the projection grammar; the tool-menu fold already needs it implicitly |
| Scope-carried config cascade (nearest enclosing scope supplies model/prompt — placement by context) | design session | defer — per-node config facts cover the doc 21/22 cases (subagents are separate nodes); revisit when one node genuinely fires under different scopes |

### C. Deferred by explicit decision

- **Federated log writer.** The single-writer log buys back ~80% of
  Naiad's verified protocol ([17](./17-quiescence-prior-art.md));
  federating it is a deliberate future entry into that zone, never an
  incidental scaling step.
- **Multi-host transport and authentication** ([09](./09-embedding-api.md)):
  deployment-boundary concerns, out of scope for v1.
- **Per-request permissions** ([06](./06-permissions-and-scopes.md)
  non-goal): the grant vocabulary doesn't change when it lands.
- **Legacy roadmap phases pending re-derivation on the converged
  model**: 4e (embedder API — its wait-predicate/projection surface
  predates doc 19's retirements), 5 (embeddings), 6/7 (optimiser and
  archmotif-as-subscriber — re-founded as changeset clients by
  doc 20; the vision stands, the mechanics moved).

### D. The standing test

Any proposed addition must survive the question the whole model is built
on: *is this a new primitive, or a convention over Event / Reaction /
Projection?* Docs [18](./18-pipeline-walkthrough.md) and
[21](./21-operator-exercise.md) are the evidence that the pressure lands
on naming and topology, not on primitives — a result to defend, not just
to enjoy.
