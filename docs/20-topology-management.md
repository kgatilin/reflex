# 20 — Topology management: the control plane on the converged model (DRAFT)

> **Status: DRAFT / proposed.** Re-founds
> [05-control-plane-as-events.md](./05-control-plane-as-events.md) on the
> converged model: the daemon's management surface — add / change / remove
> nodes, subscriptions, scopes, and projections at runtime, every operation
> graph-validated. Doc 05's thesis (subscriptions are events, the live
> table is a fold) survives intact and shipped (Phase 4b/4c); what changes
> is the vocabulary (subjects), the mutation path (an event pipeline
> instead of a synchronous bus-edge API), the unit of change (changesets),
> and the surface (scopes and projections are topology too). Builds on
> [15](./15-primitive-reduction.md)–[19](./19-projections.md).

## What doc 05 already settled, and what it predates

Settled and kept: wiring is first-class events; the live table is a fold
of the control-plane stream; YAML is a seeded stream; the cycle check runs
on every mutation, boot or runtime; audit is an ordinary subscriber;
rejection is a recorded event. None of that moves.

Predated: the event names (`HandlerRegistered`, not subjects); mutation as
a synchronous API at the bus edge (`SubscribeWithCheck` / `SubscribeAs`)
rather than events; per-operation validation (which cannot express the
six-event merge of doc 05 safely); and a management surface of handlers +
subscriptions only — while the model of docs 16/19 made **scopes**
(budgets, deadlines, closure predicates) and **projections** configuration
of equal rank.

## The grammar: requested → facts | rejected

A client — CLI, optimiser, hot-reload watcher, plugin — never mutates the
topology. It **requests**:

```
sys.topology.changeset.requested{ ops: [...], principal }
   → engine validates: fold(live table) + ops → resulting graph
   → facts:  sys.node.registered · sys.subscribed · sys.scope.declared · …
     + sys.topology.changeset.applied{ id }
   | sys.topology.changeset.rejected{ id, reasons: [...] }
```

**The live table folds only facts, and only the engine writes facts.**
This is the control plane's uprightness rule — the exact parallel of
"`caused_by` is dispatcher-stamped" (doc 17): no client can claim a
subscription into existence around the validator, because the claim kind
is not in any client's emit set. The principal-attributed synchronous
methods of Phase 4c (`SubscribeAs` et al.) become sugar over this event
pair; the permission check (doc 06) runs on the `requested` event's
principal before validation.

## Changesets: the unit of validation and atomicity

Doc 05's handler merge is six events "as a single transaction" — but
per-operation validation breaks it mid-sequence: after unsubscribing `A`
and before subscribing `AB`, the graph has an orphan gap that a
per-step checker must either reject (deadlock) or admit (window of
inconsistency). Hence:

- **The resulting graph is validated, not each step.** The validator
  applies all ops to a copy of the fold and checks the *outcome*.
  Intermediate states are **inexpressible** — the changeset is appended
  as one batch (trivial under the single-writer log) between dispatches,
  so no delivery ever consults a half-rewired table.
- A changeset is accepted whole or rejected whole, with the full reason
  list — not the first failure.
- In-flight work is untouched by construction: an unsubscribed node stops
  matching *future* dispatches; its open obligations complete naturally
  and the quiescence machinery (doc 17) drains them. Graceful removal is
  not a mechanism — it is obligation counting doing its job.

## One pipeline, every source of change

| Source | What it emits |
|---|---|
| boot (`reflex daemon config.yaml`) | parse → one `changeset.requested{principal: boot}` |
| admin CLI (`reflex topo …`) | a changeset from the operator's principal |
| optimiser (doc 08) | a rewrite *is* a changeset — as designed |
| hot reload | a watcher diffs desired YAML against the live-table fold → changeset (apply semantics: declarative desired state reconciled into ops) |
| plugin `hello` | the plugin's tools + projections (doc 19) as its changeset |

"Boot or runtime, same check" (doc 16) becomes literal: boot validation is
this validator; a failing boot is a `changeset.rejected` with reasons, and
the daemon refuses to start. `reflex validate` is a **dry-run of the same
changeset** — no separate validation code path exists.

## The managed surface: four object kinds

```
sys.node.registered / .deregistered           a reaction: on / emit / reads / config
sys.subscribed / .unsubscribed                an edge, incl. scope-qualified in: (doc 16)
sys.scope.declared / .updated / .retired      root, budget, deadline, closed_when
sys.projection.registered / .deregistered     name, on, in, shape, selectors (doc 19)
```

Scopes-as-events is the load-bearing extension: "raise this budget",
"change the closure predicate", "add a deadline" are validated runtime
operations, not a config-file edit plus restart. The `scopes:` and
`projections:` YAML blocks are seeded streams exactly as `nodes:` always
was.

## Validation: two severities

The model resolves a tension inside doc 16 (the dispatch section refuses
uncapped cycles at wiring time; the budget section demotes the Tarjan
check to a lint). The resolution is a severity split:

| Severity | Examples |
|---|---|
| **reject** — the resulting graph is structurally invalid | subscription by an unregistered node · `reads:` naming a nonexistent projection · `in:` naming an undeclared scope · deregistering a projection some node `reads:` · removing the mandatory session budget (breaks G7) · malformed subject in `on`/`emit` |
| **lint** — legal but suspect; announced as `sys.lint.*` events, never blocking | a cycle not covered by a *tight* budget (the mandatory session budget still bounds it — G7 holds) · an emitted kind losing its last consumer while emitters remain (future orphans — which G4 will surface anyway) · a `reads:` whose horizon can be empty (doc 19) |

Lints are events, so they are subscribable: a strict deployment registers
a reaction that escalates `sys.lint.*` into a rejection policy; a
permissive one just audits them. The severity *policy* is topology; the
severity *mechanics* are the engine's.

## Live instances: declarations pin at the root, interventions are events

A scope declaration change takes effect for **new instances**: an instance
reads its configuration at its root event's log position — deterministic
and replay-stable (G5), no mid-flight budget semantics shift.

The legitimate ops need — "extend the deadline of this stuck request,
now" — is **not** a configuration change. It is an event into the live
cone:

```
sys.scope.{instance}.budget.extended{ kind, delta, principal }
sys.scope.{instance}.deadline.extended{ delta, principal }
sys.scope.{instance}.cancelled{ principal }          (the explicit cancel of doc 16)
```

Visible, audited, causally placed in the cone it affects. Configuration
and intervention are separated: the former governs the future, the latter
touches exactly one instance, on the record. The budget/deadline folds
(doc 16) gain one input kind each; nothing else changes.

## What falls out for free

- **Graceful node removal** — unsubscribe + natural obligation drain
  (above); "drain then gone" needs no mechanism.
- **The LLM menu updates instantly** — it is a projection of
  `tool.*.call` consumers (doc 11/19), so removing a tool's subscription
  removes the tool from the model's menu on the next firing, zero config.
- **History, diff, rollback** — the log *is* the topology history.
  `reflex topo diff` = the difference of the live-table fold at two log
  positions; rollback = the inverse changeset, itself validated and
  recorded. Version control without a second store.
- **Audit** — doc 05's audit handler just gains kinds; it was already an
  ordinary subscriber.

## The daemon surface

All management is one of two verbs — emit a changeset, read a view. No
privileged channel exists (G8); the admin CLI is a client like any plugin,
distinguished only by its principal.

```
reflex topo apply  <file|ops>     emit changeset.requested, wait applied|rejected
reflex topo show   [--at <pos>]   the live-table fold (nodes, edges, scopes, projections)
reflex topo diff   <pos> <pos>    fold difference
reflex topo history               the changeset stream
reflex validate    <file>         dry-run changeset (no append on success)
reflex scopes      [--live]       progress projection: open instances, budgets, frontiers
reflex emit        …              the existing domain ingress
```

`--wait` predicates (doc 03) extend naturally: `--wait
changeset.applied`. Inspection commands are views at a position (doc 19's
read-only observer access), so they work identically against a live
daemon or a cold log.

## Amendments to earlier docs

| Doc | Amendment |
|---|---|
| 05 | event names move to subjects (`sys.node.registered`, …); the synchronous mutation API becomes sugar over `changeset.requested`; per-operation validation becomes resulting-graph changeset validation; the boot sequence's steps 2–5 collapse into one boot changeset |
| 16 | the refuse-vs-lint tension on uncapped cycles resolves via the severity split: structural invalidity rejects, missing tight budgets lint (G7 carried by the mandatory session budget); the engine-emits table gains the changeset, scope-declaration, and intervention kinds |
| 19 | projection registration rides the same changeset pipeline; `sys.projection.registered` is a fact kind written by the engine, requested like any other |

## Deltas in current code

1. **Changeset pipeline**: `sys.topology.changeset.requested/applied/rejected`
   kinds; the resulting-graph validator (fold + ops → checks); batch append
   between dispatches. Replaces per-call `SubscribeWithCheck` validation.
2. **Subject renames** of the Phase 4b/4c control-plane events
   (`HandlerRegistered` → `sys.node.registered`, …) — wire-format change,
   coordinate with the SDK.
3. **Scope and projection declarations as control-plane events** with
   `scopes:` / `projections:` YAML seeding; pin-at-root semantics for
   instances.
4. **Intervention kinds** (`budget.extended`, `deadline.extended`,
   `cancelled`) read by the budget/deadline folds.
5. **Severity split** in the validator + `sys.lint.*` announcements; the
   boot path unified through the changeset (a failing boot = rejected
   changeset).
6. **CLI**: `reflex topo apply/show/diff/history`, `reflex scopes`;
   `reflex validate` re-implemented as dry-run.

## Open / unresolved

- **Changeset concurrency.** Two simultaneous `changeset.requested` events
  validate against the same fold; the second to append may be stale.
  Lean: validate at append time under the single writer (serialized
  anyway), so the second request re-validates against the first's outcome
  — optimistic concurrency, no locks; a `rejected{reason: conflict}` is
  possible and honest.
- **Partial apply for plugin hello.** A plugin registering three tools
  where one collides: reject the whole hello (consistent with changeset
  atomicity) or admit the valid two? Lean: reject whole — the plugin
  retries with a fixed manifest; partial admission creates the
  half-rewired states changesets exist to prevent.
- **Rollback fidelity.** The inverse changeset restores the *table*, not
  in-flight instances (pinned at their roots). Acceptable — pinning means
  old instances were never governed by the rolled-back declarations — but
  the docs must say so.
- **Lint policy as topology.** The escalation reaction
  (`sys.lint.*` → reject) needs a way to participate *before* the
  changeset applies — which makes it a validator plugin, not a subscriber.
  Either accept built-in severity only, or define a pre-apply validation
  hook (a `sys.changeset.validating` round). Lean: built-in only until a
  real deployment demands otherwise.
