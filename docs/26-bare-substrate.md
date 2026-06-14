# 26 — The bare substrate: scope as a built-in projection, enforcement as graph shape, the LLM emits events not tool-calls

> **Status: DRAFT / proposed.** Converged in a design session. It pushes the
> [24](./24-concept.md) reduction one level deeper and *subtracts* in three
> places: it removes "scope" as a fourth managed kind, removes the
> dispatch-time enforcement gate, and removes "tool" as a capability the LLM
> holds. All three become conventions over the three primitives. This
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

## 3. Enforcement is graph shape, not a runtime gate

The one piece that looked irreducible — the dispatcher *refusing delivery*
for `budget_exhausted` / cancellation — is not. It reduces to
**deterministic guard nodes + a compile-time check on graph shape.** After
this move, `dispatch` sheds its "enforcing budgets" clause
([24 §4](./24-concept.md)): dispatch = stamp the trace + deliver. Both
mechanisms become pure of enforcement.

### 3a. Termination (G7) is a static graph property

A cycle terminates iff **every path that re-triggers it passes through a
deterministic node that reads a monotone counter and exits at a threshold.**
Per-firing work is already finite (an LLM response is finite; a deterministic
body emits a finite set). Only *re-triggering* — a cycle in the topology —
can run unbounded. So bound the cycles, structurally:

> **Check.** Compute the SCCs of the static topology graph (Tarjan). Every
> non-trivial SCC must contain a deterministic node that (a) reads a monotone
> counter projection scoped to the cone, and (b) has an emit-edge to an
> exit/terminal kind gated on that counter.

This *upgrades [24's](./24-concept.md) "Tarjan survives as a lint" from a
warning to the mechanism*: it is no longer "this cycle is not covered by a
tight budget" advice — it is what *replaces* the gate, a hard reject at
changeset validation. The LLM can emit as much as it wants in one firing,
but it can re-trigger itself **only through the guard**, and the guard is
deterministic and monotone. Decidable for the monotone-fold +
constant-threshold shape — which is exactly what a budget is. Richer
predicates fall back to a lint (the halting problem, named honestly).

### 3b. Deadlines and cancellation are guards over `clock.tick`

- **Deadline.** Time is `clock.tick` events ([24 §1](./24-concept.md)). A
  deterministic guard subscribes to `clock.tick` plus the cone's open state;
  on a tick past the deadline it routes to the exit kind. Events + state, no
  gate.
- **Cancellation / "cancel the rest".** Stopping *new* work is a guard on
  the re-trigger edge (the same edge §3a guards). Early closure (race,
  quorum) routes the continuation by predicate.

### 3c. Why this is a guarantee, not delegation

In a prior framing the objection was: "a guarantee cannot be delegated to
the reactions it constrains." This move defeats that objection precisely —
enforcement is delegated **not to the constrained node (the LLM) but to a
separate deterministic guard**, and *the guard's presence on every critical
edge is compile-verified*. So **well-formed graph ⇒ terminating runtime**:
the guarantee is structural, established statically, never contingent on the
constrained node's goodwill. This is the ranking-function termination proof
of program verification, recast as a graph-shape check. The failure mode it
forbids — an `llm → llm` cycle with no deterministic guard — is exactly what
the check rejects; the LLM is never on a critical enforcement edge.

### 3d. The residue — what the gate bought that guards don't

A node acts *after* an event exists, never before. So a node cannot prevent
the dispatch of an **already-in-flight** result. This leaves exactly two
things the gate did that guards do not:

1. **Wasted in-flight compute** on early cancellation — not reclaimable from
   the event plane. Already accepted: [24 §B](./24-concept.md) leans
   *"Quorum/race cancellation: accept (log records results)."*
2. **Double-continuation** when a late result lands after an early close —
   handled by an **idempotent join-consumer** (a guard on an "already fired"
   state). Idempotency is already required for effectful tools (G5,
   idempotency keys, intent-before-effect).

Net: **the gate is required for no guarantee.** Soundness holds without it.
Its only value was efficiency (reclaim compute) and ergonomics (spare
consumers from idempotency) — both already-accepted compromises. The
in-flight result, when it lands, is *recorded* (G1: the log keeps what
happened) and *deduped* by the consumer; it never re-fires the continuation.

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
| [24 §4](./24-concept.md) dispatch | "consulting the progress projection … **enforcing budgets**" | dispatch = stamp trace + deliver; enforcement is graph shape (§3) |
| [24 §5](./24-concept.md) budget hard-stop | `scope.budget_exhausted` = **refused dispatch** | a compile-mandated deterministic guard node on the cone's re-trigger edges (§3a) |
| [24 §5](./24-concept.md) cancellation | "refused dispatch read off the cone" | guard on the re-trigger edge + idempotent consumer; in-flight result recorded then deduped (§3b/§3d) |
| [24 §5/§7](./24-concept.md) managed kinds | **four**: nodes, subscriptions, **scopes**, projections | **three**: nodes, subscriptions, projections; scope = built-in projection instance + guard nodes (§2) |
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
  now a *pure projection*, read by guard nodes and the barrier reaction —
  not by a gate.
- **`caused_by` stamping survives** (uprightness, [24 §2](./24-concept.md)):
  membership-is-geometry depends on it; without engine-stamped causality the
  cone has no definition.
- **The validator gains teeth, not a primitive.** The SCC-guard check (§3a)
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
| enforcement → graph shape | replaces a runtime privilege with a static check + deterministic guards (ordinary nodes) | no new primitive, a stronger validator; passes |
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
  the closure algebra the guards re-express.
- [15-primitive-reduction.md](./15-primitive-reduction.md) — the two-body
  vocabulary; the `router` reaction the value-fork rides on.
- [19-projections.md](./19-projections.md) — the fold grammar a scope is now
  an instance of; horizons.
- [20-topology-management.md](./20-topology-management.md) — changeset
  validation, where the SCC-guard check lands; per-instance interventions.
