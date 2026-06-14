# 26 — The bare substrate: scope as a built-in projection, termination is a scope budget, the LLM emits events not tool-calls, the event catalog is self-hosted

> **Status: DRAFT / proposed.** Converged in a design session. It pushes the
> [24](./24-concept.md) reduction one level deeper and *subtracts* in three
> places: it removes "scope" as a fourth managed kind, moves cycle-bounding
> off a per-event dispatch gate onto a scope budget (enforced by a per-scope
> cap, with a terminal `budget_exhausted` fact for graceful shutdown — §3d),
> and removes "tool" as a capability the LLM holds. All three become
> conventions over the three primitives. It then *adds* one thing — an **event
> catalog** (`kind → schema`) on a type axis orthogonal to the wiring,
> self-hosted as a projection over `event.registered` (§4a) — without a new
> primitive. It also fixes the per-scope state model: **one state per scope**,
> written locally, promoted to a parent only through closure (§2a). This
> supersedes specific clauses of [24 §4/§5/§7](./24-concept.md) and restates
> one assumption of [25 §6](./25-regulation-concept.md); the supersession
> table is §5. It adds no primitive — every move is the standing test
> ([24 §D](./24-concept.md)) applied harder than before.

---

## 1. Thesis: three primitives, two mechanisms, nothing privileged below them

Restate the settled model ([24 §1](./24-concept.md)) at the lowest level,
in the operator's words:

```
Layer 0 — substrate:  the log + append. Events. States = projections (folds
                      over the log). Reactions (subscribers reading states,
                      emitting events). Nothing else.
The two mechanisms (24 §4):  append (the sole write) and dispatch (stamp the
                      trace, deliver to the live table). Nothing else.
```

Everything the engine appears to *do extra* — scopes, barriers, budgets,
deadlines, cancellation — is a **built-in instance of the three
primitives**, declared and maintained by the engine itself and surfaced as
ordinary states and events. The user vocabulary stays at two reaction
bodies (`llm`, `tool`); the engine's built-ins are *more instances of the
same three primitives*, not new kinds. The operator sees state (updating →
final) and events; the engine authors some of that state, but it is state
on the same log, foldable by anyone (G8, the uprightness rule of
[24 §2/§7](./24-concept.md)).

This document carries that sentence to its conclusion in three steps.

## 2. Scope is a built-in projection, not a fourth managed kind

[24 §4](./24-concept.md) already says the load-bearing thing: *"the richness
lives in the progress projection over the `caused_by` DAG — where loops,
barriers, budgets, cancellation, and orphan detection all come from."*
Finish the sentence: **a scope *is* that projection, plus the events it
drops on the log.** It is not a thing alongside projections; it is one.

Decompose a scope and every part lands on a primitive:

| Part of "scope" | Reduces to |
|---|---|
| membership / cone geometry (the three-clause rule, [24 §5](./24-concept.md)) | a fold over `caused_by` — a **projection** |
| obligation count (quiescence detection) | a counting fold — a **projection** |
| `scope.closed` / `budget_low` / `orphaned` | a built-in **reaction** emitting at the fixpoint / threshold crossing ([24 §1](./24-concept.md): "threshold crossings are announced back onto the log as events") |
| sealing — "the next firing never emits into the closed cone" | already *geometry, not discipline* ([24 §5](./24-concept.md) second clause): the closure's consumer lives in the parent cone by construction. No mechanism. |

**Consequence for [24 §7](./24-concept.md).** The "four managed object
kinds — nodes, subscriptions, scopes, projections" loses one. **Three
remain: nodes, subscriptions, projections.** A scope is a *built-in
projection instance*; its budget/deadline/closure-predicate are config on
that instance (already runtime-editable log facts via the interventions of
[20](./20-topology-management.md)/[24 §5](./24-concept.md)), not a separate
object class.

**The operator's framing.** `scope.X.closed` is the engine's built-in
progress-projection reaching its *final value*, announced as an event. The
"state finality" intuition is exactly right: a region is final when its
accumulating state stops changing because no pending reaction will write
into it again — which is quiescence, which is the obligation count hitting
zero.

**But closure is not the same as completion.** An earlier draft said
"closure and state-fixpoint are one thing observed twice"; that holds *only
when the reconciler converged*. Pull them apart:

- **Quiescence** — the cone froze: obligations are zero, no further event
  will arrive.
- **Terminality** — the frozen value is a *terminal* value (`done` /
  `needs_clarification`).

A cone can freeze on a **non-terminal** value (`gathering`, no answer yet) —
that is a **stall**: mechanically quiet, but the reconciliation did not
converge. "State stopped changing" ≠ "state reached a terminal value." The
gap between *frozen* and *done* is exactly where an LLM bridge attaches:
`scope.X.closed` is an engine-emitted kind, and a stalled close is a
**dead-end in the connectivity sense** ([27 §5](./27-state-defined-agent.md)) —
its bridge reads the frozen state and either emits a terminal event or
**re-drives** (emits the next work event). Re-drive does **not** reopen the
closed cone (closure is monotone, or the fold drifts): the bridge's emission
lives in the parent cone (the sealing row above) and opens a **new child
cone**. The "follow-up loop" `cone₁ closes (stall) → bridge → cone₂ opens →
…` *is* the cycle that a budget bounds (§3f).

### 2a. One state per scope; writes are local; promotion is through closure

The "accumulating state" above is not a vague notion — it is **one state per
scope instance**, the scope's single canonical record. It is a built-in
projection: the fold of the cone's `state.updated.{path}` events into a kv
(`path → payload`). It is born with the scope (the root event) and frozen at
closure — **state lifecycle is scope lifecycle**, so "who creates state" has
no separate answer: rooting the scope *is* creating its state. The engine
stays payload-blind: it keys by the path token in the subject and stores the
bytes; meaning is the reconciler's.

- **One per *instance* → isolation is free.** N parallel `request` cones are
  N `task_state`s, each seeing only its own ([27 §4](./27-state-defined-agent.md)).
  `task_state` is **not** "a projection over the log" — it **is** the
  `request` scope's one state.
- **Declared projections are *views over states*, never a second state.**
  Many views over one state (a `goal` slice; `context` as a list; a join of
  `request` state with `global` state). A view reshapes; the underlying
  per-scope state stays single. This is what a node's `Reads` names.
- **Writes are local.** A node's `state.updated.X` writes **only its own
  `In` scope's state** — no ancestor addressing, no `state.updated.{scope}.{path}`.
  Simple, and it keeps "what wrote this" answerable from the cone alone.

**Promotion to a parent (incl. global) goes through closure — and only
through closure.** A mid-cone emit is `caused_by` a cone event, so it lives
*in that cone* and dies with it; it cannot durably be a parent's/global's
state. The **only** event that is both caused by the cone and placed in the
parent is `scope.X.closed` (the sealing row above puts it in the parent).
So: closure **carries the cone's final state snapshot** in its payload (the
engine attaches its own maintained fold, as it does the obligation count),
and a **parent-scope consumer** of `scope.X.closed` folds the chosen fields
into the *parent's* state — an ordinary reaction, the same consumer-of-closure
as the join/barrier and the stall bridge. State flows **up through closures,
never sideways through scopes**: the reduce step of a map over child cones.

A **live** cross-scope write is impossible without breaking an invariant we
hold: it would have to strip `caused_by` (re-root the event out of its cone)
or declare a subject class ambient despite its causality (a sender claim —
exactly what the [24 §2 amendment](./24-concept.md) "membership is topology,
not a sender claim" forbids). So there is no live up-write; the old
`sys.state.updated.*` promotion of [27 §4](./27-state-defined-agent.md) is
retired in favour of closure-propagation.

**Latency is a granularity knob, not a second mechanism.** Promotion is
always on-close, but *close can be as fine as you make it*: wrap the unit you
want shared (one "read a file + index it") in a small sub-scope and its
closure promotes near-live, without waiting for the whole `request` to close.
Promotion latency = scope granularity; the mechanism stays one.

**What does *not* dissolve.** The *region itself*. "Final" is meaningless
without "final over **what**" — and the answer is the cone: a root event
plus the events `caused_by`-descended from it up to the closure. The cone is
not removable; it is what makes finality mean something, and it is
load-bearing for four independent jobs (the join/barrier, the budget
boundary, the cancellation blast radius, the isolation horizon). But "the
causal region a finality condition ranges over" need not be a *primitive* —
it is a projection's horizon. The word "scope" survives only as a name for
that region; it leaves the list of primitives and the list of managed kinds.

## 3. Termination is a scope budget, not a node property

The one piece that looked irreducible — bounding cycles so the system
provably halts — does **not** rest on classifying nodes. An earlier draft of
this section hung it on "deterministic guard nodes"; that was wrong.
**Determinism does not give termination.** Two deterministic reactions
`A: on Y → emit X` and `B: on X → emit Y` ping-pong `X → Y → X → …` forever —
a cycle of deterministic nodes is no more bounded than a cycle through an
LLM. The node's nature is irrelevant.

What bounds a cycle is a **scope budget** ([24 §5](./24-concept.md), "loops
are budgets"): a fold counts a kind's occurrences within a cone, and the
loop *is* that count. Termination is a property of the **scope that covers
the cycle**, never of a node on it. `dispatch` still sheds its *per-event*
"enforcing budgets" clause ([24 §4](./24-concept.md)) — but the bound moves
up to the scope, not out to node attributes.

### 3a. The static check: every cycle is covered by a budgeted scope

> **Check.** Compute the SCCs of the static topology graph (Tarjan). Every
> non-trivial SCC must be **covered by a scope that declares a budget**
> bounding a kind on the cycle — otherwise reject.

This *upgrades [24's](./24-concept.md) "Tarjan survives as a lint" from a
warning to the mechanism*: "this cycle is not covered by a tight budget"
stops being advice and becomes a hard reject at changeset validation.
Decidable (SCCs + scope coverage), and it needs **no node attribute** —
there is no `llm`/`deterministic` tag in the model (`Node.Kind` does not
exist). The reflex step-1 validator approximates "covered" as "every SCC
node is `in:` a budgeted scope"; the precise rule — the budget bounds a kind
on the cycle's edges — is a documented refinement.

### 3b. Deadlines are the same mechanism over `clock.tick`

Time is `clock.tick` events ([24 §1](./24-concept.md)); a deadline is a
wall-clock budget crossed by ticks — the same scope property as 3a.
Cancellation ("cancel the rest" in a race/quorum) is the covering scope
closing early; stopping *new* work is its budget ceasing to admit the
bounded kind.

### 3c. Why this is a guarantee, not delegation

The bound is structural because the **count is a fold the engine maintains**
(the progress projection, §2), not a number a node computes and might ignore.
An LLM cycle and an all-deterministic cycle are bounded identically, by the
covering scope. Nothing is delegated to the reactions the bound constrains —
it lives one level up, on the scope. This is *why* `Node.Kind` does not
exist: the engine never needs to know whether a reaction reasons or computes.

### 3d. How the budget bites at the threshold — resolved: cap is the guarantee, the fact is graceful

What the budget *counts* is settled (a scope fold); how it *acts* at the
threshold has two shapes — and they are **not either/or**, they have
different jobs:

- **Scope cap** (per-scope) — past the threshold the engine stops *admitting*
  the bounded kind into the cone: dispatch reads the scope-state and a
  budget-exhausted cone delivers no more of the counted kind. This is the
  **guarantee**. It is the one runtime gate kept, and it *partially walks
  back* "no gate at all" honestly: the per-event gate is gone; a per-scope
  cap remains.
- **Terminal `budget_exhausted`** (a fact) — the threshold drops a fact no
  node in the cycle consumes. This is **graceful shutdown**, not the
  guarantee: a loop node may subscribe and emit an honest
  `needs_clarification` instead of going dark, and the cone records *why* it
  closed.

Why not terminal-only: a fact a node must *honor* to stop the loop is exactly
the delegation §3c forbids — the node can ignore it and loop on. Only the cap
(the engine ceasing to deliver) is structural. So the cap floors the
guarantee; the fact decorates it.

This unifies with closure (§2): `closed` now has **two predicates** into its
final value — `obligations == 0` (natural quiescence) or
`counted_kind == budget` (forced). Same final state, two roads; dispatch
reads the result and a final scope admits nothing. There is no moment of
decision, only two computed predicates over the cone.

**One span roots at most one scope; a scope carries many budgets.** A scope's
`Budget` is a *map* of kind → ceiling, so several budgets live in one scope
over one cone (e.g. `{llm.call: 10, tool.pay.call: 3}`). There is therefore
never a reason to root two scopes on the same event: two scopes co-rooted on
one span would share an *identical* cone — same root, same membership, same
obligation count, quiescing in lockstep — and add nothing over the single
scope with both budgets. So co-rooting is **rejected at validation** (a span
roots at most one scope); "two budgets over one cone" is one scope with two
`Budget` entries. (An inter-closure dependency between two such identical
cones would be vacuous anyway: B closes iff A closes.)

### 3f. The compile-time termination guarantee: connectivity × budget

A closed scope must lead **somewhere** — a continuation or a terminal event;
no quiescence in the void. This is checkable statically, and it is the
composition of the two checks the model already has:

| Check | Guarantees | When |
|---|---|---|
| **connectivity** (`scope.X.closed` treated as an emitted kind) | every closed scope has an **exit** — a consumer of `scope.closed` (an LLM bridge / a deterministic terminator), or a provably terminal close | compile ([27 §5](./27-state-defined-agent.md)) |
| **budget** (§3a) | that exit is **reached in bounded steps** — the follow-up loop `scope.closed → bridge → re-drive → scope.closed` is itself a cycle, so it must be covered by a budgeted scope | compile (coverage) + runtime (fold) |

Neither suffices alone: connectivity alone admits an unbounded follow-up
loop; a budget alone admits a freeze in the void. **Together they prove
every scope reaches a terminal event in finite steps** — the termination
guarantee for the whole reconciler. The engine never introspects "is the
state terminal" (it stays payload-blind); the bridge judges "done?" as it
judges everything, and the *wiring* that a judgment-or-terminal exists is
what compilation checks. Default: treat `scope.closed` as a dead-end needing
a bridge unless a close is provably terminal-only (a liveness optimisation,
stronger than reachability — not the base case).

### 3e. The residue — what a gate would buy that a budget doesn't

A reaction acts *after* an event exists, never before, so it cannot prevent
the dispatch of an **already-in-flight** result. Two consequences, both
already accepted: **wasted in-flight compute** on early cancellation (not
reclaimable; [24 §B](./24-concept.md) leans "accept — the log records
results"), and **double-continuation** when a late result lands after early
close (handled by an idempotent join-consumer; idempotency is already
required for effectful tools, G5). The bound itself needs none of this.

## 4. The LLM has no tools — it has events it may emit

The previously non-obvious point, now stated flat: **an `llm` body does not
"have tools" and does not "call" anything.** It has an **emit allowlist**
(`Actions` in the body config) and it emits events from that allowlist. That
is the whole contract.

- A "tool call" is an **emission whose kind (`tool.X.call`) happens to have a
  consumer that is a `tool` node.** From the LLM's seat there is no
  difference between emitting `tool.fs.read.call` and emitting `llm.message`
  (the text answer) — both are allowlisted emissions. *"Tool calling" as a
  distinct capability dissolves.*
- There is **no separate "menu" concept.** What the model may emit *is the
  node's `Emits` allowlist* (`Actions`) — the same `Emits` every node already
  declares, LLM or not. The "menu" advertised to the model is simply that
  allowlist; an LLM node is not special, it is a node whose body happens to
  call a model. (Connectivity is orthogonal: a kind in `Emits` with no
  consumer is an *orphan lint*, but it is still emittable and still in the
  allowlist — whether anything acts on it is graph shape, not a menu
  mechanism.)
- **What the model needs that we don't yet have: the event's *type*.** Each
  emittable kind has a **payload schema** — "emit `tool.fs.read.call` with
  `{path: string}`". The payload *is* the event's type. Today `Emit.Payload`
  is an untyped `json.RawMessage` and `Emits` is bare kind names, so the
  schema lives nowhere in the substrate; the body cannot tell the model the
  shape to emit. The missing piece is a **kind → payload-schema** association
  in the substrate — the **event catalog** (§4a) — which is exactly what
  `provider.ToolSchema` was carrying ad hoc. Lifting it to "the schema of an
  event kind" removes the last place "tool" looked special: a schema'd
  allowlist is all the model is handed.
- Provider-level **function-calling is transport encoding, not a
  mechanism.** The adapter decodes the model's function-call into
  `Emit{ Kind: "tool.X.call", Payload }` and decodes a text completion into
  `Emit{ Kind: "llm.message", … }`. Both paths produce Emits from the
  allowlist; the native-tool-call decode shipped in stage 0 (`pkg/provider`,
  [23](./23-bootstrap-roadmap.md)) is exactly this transcoding.

**Consequences.**

- The `llm`/`tool` relationship is flatter than "reasoner that calls actor"
  ([24 §3](./24-concept.md)). They are two reactions: one emits allowlisted
  events, the other consumes some of them. The coupling is **ordinary
  subscription, not a call** — there is no call edge in the model, only
  emit → subscribe.
- `allowlist ⊆ emit` upper bound: emitting outside the allowlist is the
  body's own `.failed` ([24 §2 amendment](./24-concept.md)); exceeding the
  declared `emit:` is a lint ([24 §A.4](./24-concept.md)). Both already in
  the model.
- **Crystallization is seamless** ([24 §8](./24-concept.md)): replacing an
  LLM seat with a deterministic node changes *who emits* `tool.X.call`, not
  *what a tool call is*. There was never a "call" to reimplement — only an
  emitter to swap.

## 4a. The event catalog — kinds and their schemas, self-hosted

Events have **types**: the type of an event is its payload schema. The
vocabulary of `kind → schema` is the **event catalog**, a declaration surface
distinct from the topology. Two sides, referenced against each other: declare
*what events exist* (the catalog), declare *who reacts* (the subscribers);
`On`/`Emits` name catalog kinds. The schema lives with the **kind**, never
the emitter — one source of truth, because a consumer needs a kind's shape
regardless of who produced it.

- **Not a fourth primitive, not a fourth wiring-kind — a *type axis*.** The
  catalog is the type layer over the Event primitive, orthogonal to the
  wiring (nodes/subscriptions/projections, which say *who reacts*). The
  catalog says *what facts mean* — the schema of the log, the data dictionary.
  It does not compete with the three wiring-kinds; the wiring references it.
- **Self-hosted via `event.registered`.** Registering a kind is itself an
  event: `event.registered{kind, schema}`. The catalog is the global-horizon
  **projection** folding these into `kind → schema`. Emit a registration → the
  kind becomes valid / emittable / advertised; schema evolution is a later
  registration (last-writer-wins; a "breaking-change → reject" tightening is a
  later knob). Recomputable from the log, no privileged plane (G8). At the
  substrate level it is **Event** (registrations) + **Projection** (the fold) —
  the primitive count is unchanged; the standing test passes (§7).
- **Bootstrap axiom.** `event.registered` is the one **primordial** kind whose
  schema the engine knows natively (the schema-of-schemas, like `$schema`). It
  cannot register itself; it is the seed. One axiom, everything else through
  the catalog.
- **Dynamic registration falls out.** Registering a tool plugin = emit
  `event.registered` for its `tool.X.call` / `tool.X.result` (+ schemas) and
  add a consumer node — both log facts / one changeset. "The menu updates when
  a plugin registers" (§4) becomes literal: the registration grows the
  catalog, the node decl adds the consumer, the next `llm` firing sees the new
  `Emits` kind *with its schema*. The catalog is one projection in the family
  of **management projections** (topology projection, catalog projection) —
  the self-hosting management plane of [22](./22-bootstrap-self-hosting.md)/[23](./23-bootstrap-roadmap.md).
- **`On` patterns, `Emits` concrete.** A subscription's `On` is a pattern
  matching a *family* of catalog kinds (`tool.fs.*.result`); a node's `Emits`
  resolve to *concrete* catalog kinds (it declares exactly what it produces),
  so the schema is retrievable for model-advertisement and payload validation.
- **Validation gains three catalog checks** ([27 §5](./27-state-defined-agent.md)):
  every `Emits` kind exists in the catalog (else **unknown-kind**); every `On`
  pattern matches ≥1 catalog kind (else a **dead subscription**); and (runtime)
  an emitted payload **conforms** to its kind's schema. Validation runs against
  the catalog *snapshot* at each changeset — dynamic registration is just
  another changeset the validator re-runs against, so "compilation says
  ok / not-ok" still holds.
- **The LLM stops looking special, finally.** The body advertises its `Emits`
  allowlist, each kind paired with its catalog schema. No `ToolSchema`, no menu
  mechanism — a schema'd allowlist is all the model is handed (§4).

## 5. What changes in the canon

| Clause | Before | After (this doc) |
|---|---|---|
| [24 §4](./24-concept.md) dispatch | "consulting the progress projection … **enforcing budgets**" | dispatch = stamp trace + deliver; the *per-event* budget enforcement leaves dispatch — cycle-bounding moves to a scope budget (§3) |
| [24 §5](./24-concept.md) budget hard-stop | `scope.budget_exhausted` = **refused dispatch** | a compile-mandated scope budget covering every cycle (§3a), enforced by a per-scope **cap** (the guarantee) with a terminal `budget_exhausted` fact for graceful shutdown (§3d); plus a compile check that every `scope.closed` has a continuation or terminal exit (§3f) |
| [24 §5](./24-concept.md) cancellation | "refused dispatch read off the cone" | the covering scope closes early + idempotent consumer; in-flight result recorded then deduped (§3b/§3e) |
| [24 §5/§7](./24-concept.md) managed kinds | **four**: nodes, subscriptions, **scopes**, projections | **three**: nodes, subscriptions, projections; scope = built-in projection instance (§2) |
| [24 §7](./24-concept.md) event types | implicit / untyped payloads | an **event catalog** (`kind → schema`) on a *type axis* orthogonal to wiring — self-hosted as a projection over `event.registered`, no new primitive (§4a) |
| [24 §3](./24-concept.md) LLM/menu | "emits typed actions from an allowlist; menu is a projection of consumers" | strengthened: the LLM has **no tools**, only allowlisted emissions; tool-call = emission with a tool-node consumer; function-calling = transport encoding (§4) |
| [25 §6/§7.1](./25-regulation-concept.md) | "the engine refuses dispatch on an exhausted budget" | per-cone hard-stop is a guard node (§3a); **resolver admission control is unaffected** — it is already a guard-style reaction, not the dispatcher gate |

[25](./25-regulation-concept.md)'s economic regulation is otherwise intact:
its admission gate lives at the *resolver* (a reaction refusing to mint
`request.received`), never at the dispatcher, so it survives this change
verbatim. Only the line attributing the per-cone hard-stop to a dispatch
gate needs the restatement above.

## 6. The honest floor — what this does *not* remove

- **The progress projection survives.** Obligation counting is still how the
  barrier knows a cone has quiesced and when to fire `scope.closed`. It is
  now a *pure projection*, read by the barrier reaction and consulted by the
  scope-budget coverage check — not by a gate.
- **`caused_by` stamping survives** (uprightness, [24 §2](./24-concept.md)):
  membership-is-geometry depends on it; without engine-stamped causality the
  cone has no definition.
- **The validator gains teeth, not a primitive.** The SCC budget-coverage check (§3a)
  becomes a hard reject where [24](./24-concept.md) had a lint. New
  *validation*, same three primitives.
- **The clock is load-bearing.** `clock.tick` as an event source (already
  named in [24 §1](./24-concept.md)) is what makes deadlines expressible
  without a gate.

## 7. The standing test

Run [24 §D](./24-concept.md) over all three moves — *new primitive, or
convention over Event / Reaction / Projection?*

| Move | Effect on the concept count | Verdict |
|---|---|---|
| scope → built-in projection | removes a managed kind (4 → 3) | subtracts; passes |
| termination → scope budget | replaces a per-event runtime gate with a scope budget + a static cycle-coverage check (no node attribute) | no new primitive, a stronger validator; passes |
| LLM emits events, not tool-calls | removes "tool" as a held capability | subtracts; passes |
| event catalog (kind → schema), self-hosted | adds a *type axis* over the Event primitive: registrations are Events, the catalog is a Projection, one primordial seed kind (§4a) | no new primitive; a type layer the wiring references; passes |

The first three **subtract**; the catalog **adds a type layer without a new
primitive**. The reduction direction the whole model is built on is not just
preserved — it advances. The substrate is exactly what the
lowest level should be: events, states, reactions, and one privileged
authorship rule (the engine writes the facts it owns). Scopes, barriers,
budgets, deadlines, and tool menus are built-in *instances* of those three
primitives — none of them a fourth thing.

## See also

- [24-concept.md](./24-concept.md) — the settled model this deepens; §1
  (three primitives), §3 (two bodies + menu), §4 (two mechanisms), §5
  (scopes/quiescence/budgets/cancellation), §7 (managed kinds), §B (accepted
  leans incl. wasted in-flight work), §D (the standing test).
- [25-regulation-concept.md](./25-regulation-concept.md) — economic
  regulation; its resolver-level admission gate is unaffected, its
  dispatch-gate attribution is restated here (§5).
- [17-quiescence-prior-art.md](./17-quiescence-prior-art.md) — obligation
  counting, the progress projection that survives as a pure projection.
- [16-engine-architecture.md](./16-engine-architecture.md) — dispatch and
  the closure algebra scope budgets re-express.
- [15-primitive-reduction.md](./15-primitive-reduction.md) — the two-body
  vocabulary; the `router` reaction the value-fork rides on.
- [19-projections.md](./19-projections.md) — the fold grammar a scope is now
  an instance of; horizons.
- [20-topology-management.md](./20-topology-management.md) — changeset
  validation, where the SCC budget-coverage check lands; per-instance interventions.
