# 31 — The meta-agent, and isolation via root scopes (problem + thinking)

> **Status: IMPLEMENTED (3 increments) + working notes.** The model below is
> built: (1) the `Detached` scope property + parent-free `admit`, (2) scope-aware
> validator edges, (3) foreign-scope-only changesets. See the boxed
> "Implemented" notes in §4 and the per-increment summary in §5. Captures a
> concrete problem found while
> building the *architect* — a meta-agent that BUILDS a sub-topology at runtime
> (via the in-graph control plane, [doc 20](./outdated/) / [`CONCEPT.md`](./CONCEPT.md) §8)
> and DISPATCHES a task into it — and the design thinking about how to isolate the
> sub-computation from the meta-agent. The headline conclusion: **isolation is a
> SCOPE property (multiple root scopes), not a new "graph" partition** — because a
> graph is already a projection over the log, not a primitive. Builds on
> [30](./30-scopes-state-and-fan-out.md) (scopes/cones), [26 §2](./26-bare-substrate.md)
> (closure ≠ completion; cone membership = caused_by descent). When this doc and
> `CONCEPT.md` disagree on the settled model, `CONCEPT.md` wins; the **Open
> problems** here are not yet settled.

---

## 1. The worked example — the architect (a meta-agent)

We built an in-graph control plane: a node can DRIVE a topology changeset by
emitting `topology.changeset.requested` as an ordinary event; the engine
validates + commits it mid-drain (the same pipeline as an operator `Apply`) and
the new subgraph goes LIVE in the same drain (live-refresh). See
`engine/control_test.go`, `engine/engine.go` (`commitChangeset`, the in-graph hook
in `process`), and `nodes/changeset` (the bridge body that turns a topology
*document* into a changeset request).

On top of that we built the **architect** (`topologies/architect.yaml`): an llm
node with NO file/python/test tools — only the control plane. Its job:

1. read a task,
2. **build** a worker topology that can solve it (emit `topology.apply.requested`
   with a YAML document → the bridge → `topology.changeset.requested` → the engine
   applies it, live),
3. **dispatch** the task into the worker it built (emit `task.new`), and
4. be done — it is fire-and-forget; it does not do the work itself.

This works: the architect successfully built a worker subgraph in-graph and the
mechanism (build → applied/rejected feedback → retry → dispatch) is proven. The
problem is what the worker's events do to the architect.

---

## 2. The problem — a dispatched sub-computation is NESTED in the meta cone

Cone membership is the caused_by descent (doc 24 §5 / doc 26 §2): an event is a
member of every cone its causes are members of. The architect's `brain` emits
`task.new`; the worker it dispatches is causally **under** that emit
(`task.new → request.received → worker → tool.* → …`). So **the entire worker run
is nested inside the architect's cone.** Two consequences, both real:

**(a) Nested same-kind delivery (runtime).** The architect brain (`in: architect`)
and the worker (`in: request`) both speak `llm.message`/`llm.empty` — the llm
body's default turn kinds. Because the worker cone is nested in the architect
cone, the worker's `llm.empty` IS a member of the architect cone, so a parent
subscription (`in: architect`, `on: [llm.empty]`) **receives the child's event.**
This is not a validator artifact — it is how nested cones deliver. A parent scope
catches a child scope's same-kind event.

**(b) Cross-scope unbounded cycle (static validator).** The connectivity validator
(`engine/validate.go`) draws edges by kind, scope-blind. With the brain
subscribing to a kind the worker emits, it sees
`brain → task.new → resolver → worker → … → llm.empty → brain` — a cycle through
GLOBAL nodes (`resolver`, `terminal-on-done`) that sit in no single budgeted
scope, so it is reported as an unbounded cycle and the changeset is **rejected**.

### The band-aid we used

`topologies/architect.yaml` currently sidesteps both by (i) giving the architect
brain DISTINCT turn kinds (`architect.message`/`architect.empty`, via the llm
body's `answer`/`empty` config) and (ii) making the brain fire-and-forget (it
subscribes to no worker kind). That validates and runs — but it loses **kind
reuse** (every sub-topology must rename `llm.*`) and it is a workaround, not a
model. The real question: *how do we run an independent sub-computation in one
runtime without its events bleeding into the meta-agent's scope?*

---

## 3. Wrong framing — "multiple independent graphs"

First instinct: partition the runtime into N independent *graphs*, each validated
and dispatched in isolation; the architect lives in graph `control`, builds graph
`worker`, and injects the task into `worker`.

**This is the wrong primitive.** A *graph* (the topology / live table) is already
a **projection over the log** — `foldTopology(log)` (doc 20 / G8). It is not a
thing the runtime *has*; it is a thing the log *folds to*. Introducing a `graph`
axis as a real partition would add a fourth primitive next to the event log,
subscribers, and projections — exactly the kind of special plane the substrate
model refuses (`CONCEPT.md` §1–§3). The catalog is global; the log is one. So
"many graphs" mis-locates the isolation in a new partition instead of in the
mechanism we already have.

---

## 4. Right framing — isolation is a SCOPE property: multiple ROOT scopes

The isolation we want is exactly what a **scope** is: a cone over the log. The fix
is not a new partition; it is to let the worker run in its **own ROOT scope** — a
**top-level** cone, a *sibling* of the architect's cone, not a child of it.

Today, scope membership = caused_by descent, so a cone rooted by an
internally-caused event (`request.received`, caused by the architect's `task.new`)
inherits all the parent cones. A **root scope** breaks that inheritance: rooting an
instance of a root scope starts a **fresh** membership — the instance is
top-level, a member of no parent cone — **even when its trigger is internally
caused.** The causal link (`task.new → request.received`) is preserved on the log
for lineage and trace; only the *scope membership* is detached.

This is precisely the status the architect's OWN scope already has — it is rooted
by `task.architect`, an external event with no cause, so it is top-level by
default. We generalize that from "rooted by an external event" to "**declared a
root scope**, hence top-level regardless of what triggered it."

### What it buys

- **(a) dissolves** — the worker's events live in the worker root cone, NOT the
  architect cone. A parent subscription no longer catches them. `llm.message`/
  `llm.empty` are reused freely: same kind, different cone, no collision. No
  per-graph kind namespace needed; the catalog stays global.
- **(b) dissolves** — the architect's cone closes the moment it dispatches (it no
  longer encloses the worker run) → genuine fire-and-forget, the meta-agent
  launches and is done.
- The worker root scope is a normal budgeted cone; it terminates on its own
  `request.terminal`, which the runner waits on.

### The shape

- A scope gains a property — call it `root: true` / `detached: true` (name TBD) —
  meaning *an instance of this scope is top-level; it does not inherit its
  trigger's cones.*
- The dispatcher's `admit` (`engine/scope.go`) stops inheriting parent membership
  when rooting a root-scope instance: the new cone's membership starts from the
  rooting event alone, not from its causes' memberships.
- The cross-cone link (`task.new` emitted in the architect cone → `request.received`
  rooting the worker cone) is a **port**, not an in-graph edge: causally linked,
  scope-detached.

> **Implemented (increment 1).** The scope property is `Detached bool`
> (`engine.Scope.Detached`, yaml `detached:`), threaded through the changeset
> `scopeSpec` round-trip and `pkg/topology`. `admit` and `inheritedCones`
> (`engine/scope.go`) skip parent-cone inheritance via `rootsDetached` when the
> event roots a detached scope; detachment then propagates transitively for free
> (descendants inherit from the now-parent-free membership). Proven by
> `TestDetachedScope_ReusesKindInIsolation`: a worker reuses the meta-agent's
> `llm.message`; detached → the meta cone never receives it (leak detector = 0)
> while the worker's own in-scope consumer still fires; the nested control (same
> wiring, `Detached:false`) leaks, proving the isolation is the detachment.
> **Still open:** the validator (§4 "Identifying the port") still draws cross-scope
> edges scope-blind, so an architect that reuses `llm.*` and subscribes across the
> port would still be *rejected at apply time* even though it now runs cleanly —
> the per-component / port-cutting validator change is increment 2. The
> foreign-scope-only changeset rule is increment 3.

### Two orthogonal axes: execution isolation vs projection visibility

The isolation is on EXECUTION only. A scope is an **execution scope**: a cone that
bounds *dispatch* — a node receives only events in its own cone. That is the
isolation that frees kind reuse and kills the cross-scope cycle.

**Projections are a separate axis and need not be isolated.** A projection is a
*read* over the shared log (one log, G8); its horizon can be **global**, folding
events from NEIGHBORING execution scopes when wanted. So observation is decoupled
from execution: a meta-agent can *read* its sub-scope through a global projection
without that sub-scope's events ever being *delivered* to it as triggers. Active
(dispatch) is scope-local; passive (a view) can be global. No new node type is
needed for "watching" — it is just a projection with a wider horizon.

### A changeset may only mutate a FOREIGN scope

The meta-agent builds *another* scope, never its own. A changeset issued from
inside scope X (the `changeset.requested` event is a member of an X cone) may add
or modify subscribers/scopes that live in OTHER scopes, but **not** scope X
itself. The architect (running in `architect`) builds `request`; it cannot rewrite
the `architect` loop it is running in. This keeps the live-refresh coherent (a
running cone is never mutated out from under itself) and is the natural authority
boundary: you compose *downstream* topology, you do not self-modify.

> **Implemented (increment 3).** `selfMutationReasons(issuing, decls)`
> (`engine/changeset.go`) flags any added/modified subscriber/projection `in:X` or
> `Scope` named X when X is an issuing scope; the in-graph hook (`engine/engine.go`
> `process`) rejects before commit, so the live table is untouched and the
> requesting node hears the reasons on `changeset.rejected`. The issuing scopes are
> `scopeRuntime.scopesOf(span)` — the cones the `changeset.requested` event is a
> member of, read off the same membership fold delivery uses. An external operator
> `Apply` has no issuing cone (issuing empty), so the rule applies only to in-graph
> changesets — exactly as intended. `global`/empty targets and `Event` decls
> (catalog-global) are exempt. Proven by `TestSelfMutationReasons` and
> `TestInGraphChangeset_RejectsSelfScope`.

### Identifying the port — and why no "cut" pass is needed

> **Implemented (increment 2) — and simpler than first framed.** The validator
> does NOT cut ports or partition into components. Cycle detection (SCC) already
> handles a disconnected graph natively — Tarjan finds every SCC in one pass
> regardless of weak-connectivity — so "validate per weakly-connected component"
> bought nothing. The real defect was a **phantom edge**: the validator drew edges
> by kind-match while being scope-BLIND, so a meta-agent reusing a kind the
> sub-topology also emits got a spurious cross-scope edge → a false unbounded cycle
> through the global injector → rejection.
>
> The fix makes edge construction mirror the runtime's cone-delivery rule
> (`deliverableStatic`, `engine/validate.go`): two **top-level** scopes — `Detached`,
> or externally-rooted (`Root` produced by no node) — root sibling cones that nest
> in neither, so an event in one is never delivered to a subscriber `in:` the other,
> EXCEPT through that consumer's own root kind (the injection **port**) or via a
> `global` node (cone inherited dynamically). The phantom edge is therefore never
> drawn, and plain whole-graph SCC just works. It is conservative (only a provably
> phantom both-top-level edge is dropped; a kept edge can over-report a cycle, never
> hide one) and **dormant** for any document with ≤1 top-level scope.
> Proven by `TestValidate_DetachedScopeSuppressesCrossScopeCycle`.

---

## 5. Implementation increments + what is still open

**Settled by (a) — the scope decides, not the emit.** Detachment is a property of
the *scope* (`engine.Scope.Detached`): any instance is top-level however triggered.
The rejected alternative — marking a specific *emit* detached — would split
caused_by from membership at the emit site, the invariant doc 26 §2 leans on. With
the scope-based choice, caused_by is untouched and only scope membership detaches.

The three shipped increments:

1. **Root (detached) scopes** — `engine.Scope.Detached`; `admit`/`inheritedCones`
   skip parent-cone inheritance when an event roots a detached scope
   (`engine/scope.go`); detachment propagates transitively for free. Proven by
   `TestDetachedScope_ReusesKindInIsolation`.
2. **Scope-aware validator edges** — `deliverableStatic`/`topLevelScopes`
   (`engine/validate.go`): the phantom cross-scope edge between two top-level scopes
   is never drawn, so plain whole-graph SCC reports no false cycle. No cut pass, no
   per-component split (SCC is already component-agnostic). Proven by
   `TestValidate_DetachedScopeSuppressesCrossScopeCycle`.
3. **Foreign-scope-only changesets** — `selfMutationReasons`/`scopesOf`: an
   in-graph changeset may build OTHER scopes but not the cone it runs in. Proven by
   `TestSelfMutationReasons` + `TestInGraphChangeset_RejectsSelfScope`.

Still open:

- **Monitoring is the projection axis, not the execution axis** (see §4 "Two
  orthogonal axes"). A meta-agent that wants to WATCH its sub-computation declares a
  **global projection** that folds the sub-scope's events from the shared log and
  `reads:` it — it never subscribes (dispatch is cone-local, by design). A
  meta-agent that must *react* to (not just read) the sub-computation needs an
  explicit **port** back (a declared cross-scope trigger, the inbound mirror of
  launch), or that reaction belongs to a node inside the sub-scope / a global
  terminal handler. The fire-and-forget architect needs neither. Not yet built —
  no global-horizon projection over a sibling scope has been exercised.
- **Nested-scope edge precision.** `deliverableStatic` only suppresses an edge when
  BOTH endpoints are top-level (provably siblings). An edge from a node in a scope
  nested under top-level A to a node in a different top-level B is kept (conservative
  — could over-report, never hides a real cycle). Tightening this needs static
  nesting inference, left as future work.
- **Apply the capability to the architect.** The `architect` topology still uses the
  band-aid distinct turn kinds (`architect.message`/`architect.empty`). Marking the
  dispatched worker scope `detached: true` makes it a true sibling (genuine
  fire-and-forget); note the band-aid is also load-bearing for a *separate* reason —
  the architect's prose turn is terminal (no consumer) while the worker's
  `llm.message` is consumed, and `terminal` is a catalog-global property — so kind
  reuse across the two is constrained independently of detachment.

**No new primitive.** Build (`topology.changeset.requested`) and launch (emit the
root scope's Root kind) remain the meta-agent's two control ops; root-ness lives on
the scope, not the emit. The substrate (log + subscribers + projections) is
unchanged — the isolation moved into the scope model where it belongs.

---

## 6. Summary

- A meta-agent that builds and dispatches a sub-computation in one runtime hits
  nested-cone delivery (kind collision) and a cross-scope validator cycle.
- "Multiple graphs" is the wrong fix: a graph is a projection over the log, not a
  partition.
- The right fix is **multiple ROOT scopes**: a scope declared top-level roots a
  fresh cone that does not inherit its trigger's cones, so the sub-computation is
  a sibling of the meta-agent, not a child — isolating dispatch and freeing kind
  reuse, with no new primitive.
- **Two orthogonal axes**: an *execution scope* isolates DISPATCH (a node receives
  only its cone's events); a *projection* is a separate READ axis whose horizon can
  be global, folding neighboring scopes from the shared log. So observation is
  decoupled from execution — a meta-agent reads its sub-scope via a global
  projection, never by a subscription.
- **Built in three increments**: (1) the `Detached` scope property + parent-free
  `admit`; (2) **scope-aware validator edges** — NOT a port-cut/per-component pass:
  SCC already handles disconnected graphs, so the only fix needed was to stop
  drawing the phantom cross-scope edge between two top-level scopes; (3) a changeset
  that may only mutate a FOREIGN scope. Monitoring (a global projection over the
  shared log, not a subscription) remains the open piece.
