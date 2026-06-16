# 31 — The meta-agent, and isolation via root scopes (problem + thinking)

> **Status: DESIGN / working notes.** Captures a concrete problem found while
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
boundary: you compose *downstream* topology, you do not self-modify. Enforcement
is a check in `commitChangeset`: reject ops whose target scope equals the issuing
event's scope. (Open: an external operator `Apply` has no issuing scope, so the
rule applies only to in-graph changesets.)

### Identifying the port (entry detection) — a basic graph operation

The validator already finds entry points structurally: a node is a **root** when
it consumes a kind no node produces (`isRoot`, `engine/validate.go`). A root
scope's Root kind is an entry of the **same structural shape**, with one twist —
it may ALSO be produced by a node in a *different* component (the meta-agent's
launch). That production is a **port**, not an internal edge, so the validator
**cuts** it: a root-scope Root kind is treated as an external entry to its
component even when some node elsewhere emits it.

Two equivalent ways to mark the entry (your call):
- **Flag it** — like the existing `terminal: true` (a declared graph *output*),
  but the other way: a declared *input*. A root scope's Root kind is implicitly
  this.
- **Detect it** — structurally: a node with **0 `on` and >0 `emits`** is a pure
  input (an injector); and any root-scope Root kind is an entry. No flag needed.

With the ports cut, partitioning is a **basic graph operation**: weakly-connected
components (or directed reachability from each entry — `graphval.ReachableFromNames`,
already used here). Each component is one root scope's subgraph; validate
connectivity + the unbounded-cycle check **per component**. The architect↔worker
cycle disappears because the `task.new` port is cut at the worker's entry — they
are two components, each acyclic on its own.

---

## 5. Open problems (not yet settled)

1. **Per-component validation is the easy part** (see §4 "Identifying the port").
   Cut the root-scope ports, then run weakly-connected components / reachability
   (`graphval`, already available) and validate each component independently. The
   only real choice is how to mark the port — a declared `input` flag (the mirror
   of `terminal`) vs. structural detection (0 `on` + >0 `emits`, plus any
   root-scope Root kind). No "graph" abstraction is introduced: a component is
   just the reachable set of one root scope.

2. **How is a dispatch "detached" decided — by the SCOPE or by the EMIT?** Two
   options: (a) the *scope* is declared root (any instance is top-level, however
   triggered) — clean, declarative, what §4 assumes; (b) a specific *emit* is
   marked detached (the producer chooses to launch a fresh cone) — more flexible
   but splits caused_by from membership at the emit site, which is the invariant
   doc 26 §2 leans on. §4 prefers (a).

3. **Monitoring is the projection axis, not the execution axis** (see §4 "Two
   orthogonal axes"). A meta-agent that wants to WATCH its sub-computation does not
   subscribe (dispatch is cone-local, by design) — it declares a **global
   projection** that folds the sub-scope's events from the shared log and `reads:`
   it. Observation never re-nests the child in the parent. Open: a node only *acts*
   on a trigger, and triggers are cone-local — so a meta-agent that must *react* to
   (not just read) the sub-computation needs an explicit **port** back (a declared
   cross-scope trigger, the inbound mirror of launch), or that reaction belongs to
   a node inside the sub-scope / a global terminal handler. The fire-and-forget
   architect needs neither: it builds, launches, and is done.

4. **Relation to the control plane.** Build (`topology.changeset.requested`) and
   launch (`task.new` into a root scope) are the meta-agent's two control ops.
   With root scopes, *launch* is just "emit the root-scope's Root kind" — no new
   mechanism, because the root-ness lives on the scope, not the emit. Build is the
   in-graph changeset we already have. So the meta-agent needs **no new
   primitive** beyond the root-scope property — which is the whole point: keep the
   substrate (log + subscribers + projections), move the isolation into the scope
   model where it belongs.

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
- Settling it needs: a root-scope property on scopes; `admit` not inheriting
  parents for a root-scope instance; a changeset that may only mutate a FOREIGN
  scope; and a validator that **cuts the root-scope ports** and validates each
  weakly-connected component independently (a basic graph op — `graphval` already
  has reachability/SCCs). Monitoring is a global projection over the shared log,
  not a subscription.
