# 26 — The bare substrate: scope as a built-in projection, termination is a scope budget, the LLM emits events not tool-calls

> **Status: DRAFT / proposed.** Converged in a design session. It pushes the
> [24](./24-concept.md) reduction one level deeper and *subtracts* in three
> places: it removes "scope" as a fourth managed kind, moves cycle-bounding
> off a per-event dispatch gate onto a scope budget (the per-scope cap at the
> threshold is an open 2b question, §3d), and removes "tool" as a capability
> the LLM holds. All three become conventions over the three primitives. This
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
zero. Closure and state-fixpoint are one thing observed twice.

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

### 3d. The open question: how the budget bites at the threshold

What the budget *counts* is settled (a scope fold); how it *acts* at the
threshold is left to stage 2b. Two shapes, both scope-level:

- **Scope cap** — the engine stops admitting the bounded kind into the cone
  past the threshold. A small, principled enforcement that *partially walks
  back* this document's "no runtime gate at all": the per-event gate is gone,
  but a per-scope budget cap may be the one enforcement worth keeping.
- **Terminal `budget_exhausted`** — the threshold emits a fact no node feeds
  back into the loop, so the cone quiesces on its own.

Choosing is a 2b decision; the basis (termination is a scope budget) and the
static check (3a) hold either way.

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
- The **menu** advertised to the model is a projection:
  `allowlist ∩ { kinds with a tool.*.call consumer }`
  ([24 §3](./24-concept.md): "the LLM tool menu is a projection of the
  `tool.*.call` consumers"). Register a plugin → a consumer appears → the
  kind becomes callable → the menu updates on the next firing. Remove the
  consumer → the kind is still *emittable* but now *orphaned* (a lint), and
  it drops from the menu. Zero config.
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

## 5. What changes in the canon

| Clause | Before | After (this doc) |
|---|---|---|
| [24 §4](./24-concept.md) dispatch | "consulting the progress projection … **enforcing budgets**" | dispatch = stamp trace + deliver; the *per-event* budget enforcement leaves dispatch — cycle-bounding moves to a scope budget (§3) |
| [24 §5](./24-concept.md) budget hard-stop | `scope.budget_exhausted` = **refused dispatch** | a compile-mandated scope budget covering every cycle (§3a); how it bites at the threshold (scope cap vs terminal `budget_exhausted`) is open (§3d) |
| [24 §5](./24-concept.md) cancellation | "refused dispatch read off the cone" | the covering scope closes early + idempotent consumer; in-flight result recorded then deduped (§3b/§3e) |
| [24 §5/§7](./24-concept.md) managed kinds | **four**: nodes, subscriptions, **scopes**, projections | **three**: nodes, subscriptions, projections; scope = built-in projection instance (§2) |
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

All three **subtract**. The reduction direction the whole model is built on
is not just preserved — it advances. The substrate is exactly what the
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
