# 33 — Scopes as the composition primitive: encapsulation, channels, aspects, homeostasis

> **Status: DESIGN / agreed model, not yet built.** Captures a design session that
> reframed the primitive set and made **scope a first-class primitive** that is
> *opaque from the outside* — for whoever opens it, everything inside is "one
> node". Builds on and revises [`CONCEPT.md`](./CONCEPT.md) §2–§3 (the three
> primitives; "scope is a built-in projection") and resolves/relocates the open
> problems of [30](./30-scopes-state-and-fan-out.md) (P1–P5). Nothing here is
> coded yet; the engine changes are listed in §10 and the unresolved forks in §11.
> When this doc and `CONCEPT.md` disagree, treat **this as the newer intent** —
> `CONCEPT.md` §2–§3 are to be rewritten against it once the model is built.
>
> Legend: ✅ agreed · ⚠️ working default (revisit) · ❓ open question · 💡 note

---

## 0. The driving principle

**A scope is an opaque node.** For whoever opens it, everything that happens
inside is — as far as they can see — a single node: you hand it an input, it does
whatever internally, it hands you back an output. The opener never sees the
internals. This is the "scope = function call" analogy made literal: the root is
the call (with arguments), the closure is the return, and the body's locals are
private.

Every problem we hit while driving the architect/worker (state leaking across
scopes, a sub-scope's budget charging another phase's work, phases that wouldn't
cleanly hand off) is the *same* leak: the boundary isn't real yet. This doc makes
it real.

---

## 1. The primitive set, reframed

The current model ([`CONCEPT.md`](./CONCEPT.md) §2) lists three primitives —
**Event, Reaction, Projection** — and says ([`CONCEPT.md`](./CONCEPT.md) §3)
"scope is a built-in projection, *not* a primitive." We invert that:

```
Event⇄Reaction   the stimulus–response unit: what happened ⇄ what happens in answer.
                 Inseparable — two faces of one primitive, not two primitives.
Scope            a bounded context of execution: the unit of isolation,
                 composition, and termination. A FIRST-CLASS primitive.
View             a fold of the log into a read-model. Its HORIZON is a parameter —
                 from "this cone" (per-scope state, llm.history) to "the whole log,
                 keyed by X" (memory). The only channel that may cross a boundary.
```

Reasoning, point by point:

- ✅ **Event and Reaction are one primitive, not two.** An event is not a passive
  record — it is a *stimulus a reaction must answer*. They are the noun-face and
  verb-face of one thing. This is already encoded in the engine: the validator's
  **dead-end check** *is* "every event must have its reaction-half; an event no
  reaction answers is valid only if declared a terminal leaf." A non-reacted event
  is already treated as a bug. So "reaction is not a separate primitive" was right;
  the sharper statement is "reaction is the active half *of* Event."
- ✅ **Scope is promoted to a primitive.** In code it is already its own `Decl`; it
  gates delivery, enforces budgets, emits lifecycle facts, and owns per-instance
  state — a passive projection does none of that. Calling it "just a projection"
  is the source of the half-built feeling ([30](./30-scopes-state-and-fan-out.md)
  P5). It earns primitive status.
- ✅ **"Scope is a projection" survives, refined.** The scope's *state* is a
  built-in view (the per-scope kv it exports as output). The *scope itself* is the
  boundary that owns that view and its lifecycle. So the old intuition and the new
  framing reconcile: the state is a projection; the scope is the primitive around
  it.
- 💡 The **catalog** (`kind → schema`) stays a type axis orthogonal to wiring, not
  a primitive ([`CONCEPT.md`](./CONCEPT.md) §6) — unchanged.

---

## 2. The boundary rule — push vs pull

The whole model turns on one distinction the engine currently conflates:

- **Delivery (push)** triggers a reaction. It is **scoped / encapsulated**: an
  event emitted inside scope A is *not delivered* to any reaction outside A. The
  only thing that crosses out is the closure (the return).
- **View-folding (pull)** is a passive fold of the *log*, not of delivered events.
  It is **global**: a view may fold events from any scope, regardless of the
  delivery boundary, because reading a view triggers nothing.

> **The rule:** across a scope boundary you may **not deliver an event (push)**,
> but you **may fold the log with a view (pull)**. Delivery is scoped;
> view-folding is global.

This is why View is "the key cross-scope mechanism" — it is the *only* channel
that may cross a boundary, precisely because it is read-only and starts no
execution. It is CQRS at the substrate level: events+scopes are the command side
(encapsulated), views are the query side (cross-scope).

It also dissolves the leak worries: a child *publishing* facts that a cross-scope
view folds is fine (pull, no reaction fires, no cone entanglement). A child's
internal event *triggering a parent reaction* (push across the boundary) is the
bug — it entangles cones, breaks barriers, and charges budgets to the wrong scope.

---

## 3. The three channels between scopes

If events are encapsulated, *all* cross-scope coupling reduces to exactly three
channels — this is the interface model:

| Channel | Direction | Meaning | Mechanism |
|---|---|---|---|
| **Input** | parent → child (write) | call arguments | the root event's payload (schema = the input type) |
| **Output** | child → parent (write) | return value | closure snapshot of the scope's exported view |
| **Cross-scope view** | anyone → anyone (read) | observation / memory | a projection that folds another scope's events, read by pull |

- ✅ **Input = the root payload.** Spawning a scope is just *emitting its root kind
  with the input as payload*. No special addressing primitive — it is events all
  the way down.
- ⚠️ **Output = an exported view, snapshotted at close** (working default). The
  default exported view is the per-scope state kv (today's behaviour); a scope may
  instead declare a specific projection as its output. This removes a separate
  "output" concept (output *is* a view) and makes `output → input` composition
  type-checkable in the validator, like a function call.
- ✅ **Cross-scope view is strictly pull.** The only alternative — *subscribe to a
  cross-scope view's change* (push: A's write triggers B) — is exactly the
  boundary-crossing delivery we ban. If you want cross-scope reactivity, route an
  explicit **event** through the input channel; do not hide a subscription behind a
  view.

---

## 4. The binding axis `in` — one field, three modes

Generalise the scope-binding field `in` from a single scope name to a
scope/`*`/top-level selector. A subscription is already a **(scope × kind)**
pattern (`in` = the scope axis, `on` = the kind axis); we just let the scope axis
take more than one value:

| `in` | role | where it lives |
|---|---|---|
| `<name>` / `[names]` | ordinary reaction | inside the named scope(s) |
| `*` | **aspect** (cross-cutting) | one scope-local copy in *every* scope instance |
| *(unscoped / top-level)* | **regulator** | the top level — sees only events in no scope |

- ✅ **`in: *` is an aspect** (§5).
- ✅ **The unscoped/top-level binding is the regulator** (§6) — and this **reborns
  `global`**: the top level now sees only *unscoped* events (external inputs +
  detached outputs), **not** "everything". The old all-seeing `global` — the wart
  that let any node observe any scope — is gone.

---

## 5. Aspects — cross-cutting reactions (`in: *`)

The need: a reaction that should run in *every* scope — cost accounting,
audit/telemetry, a per-scope deadline — declared **once** and auto-attached to
every (current and future) scope instance.

Split "do X everywhere" into two distinct movements that compose:

- **A global MEASURE** (e.g. total cost of the whole run) = a **cross-scope view**:
  fold all `llm.usage` over a global horizon. Declared once, read anywhere, no
  auto-attach needed — free from §3.
- **A per-scope REACTION** (per-scope cost, a "this scope exceeded $X" guard,
  stamping cost onto each closure) = an **aspect**: a reaction declared with
  `in: *` that the engine **materialises scope-local into every scope instance**.
  In scope A it sees only A's `llm.usage`, folding into A's own state → carried in
  A's closure output. Encapsulation is intact: the one declaration is a *template*;
  each attachment is local to its host.

They compose: the aspect emits a local `state.updated.cost` in its host scope; a
global view folds cost across all scopes into the total. **"Cost everywhere" =
aspect (per-scope) + view (sum).** An aspect is *not* a fourth primitive — it is a
Reaction with the `*` binding mode.

- ❓ For exclusions ("cost on everything *except* the memory scope") the `*` axis
  needs a negative/match pattern. Syntax detail, not a model change.

---

## 6. The top level is a homeostat, not a root scope

What "holds" a long-running daemon, if every scope is a finite cone that closes?
**Nothing holds it** — there is no ambient root scope. The top level is a
**homeostat**: a control loop with a setpoint.

- A **setpoint view** measures a variable (e.g. `tasks_todo`).
- A **regulator** (a top-level/unscoped reaction) fires on deviation from the
  setpoint and **spawns a corrective execution scope**.
- "Alive forever" = *standing ready to react*, not running an open cone. The
  resting state is **quiescence at the setpoint** — nothing executes, the log just
  sits.
- An external event (`task.new`) knocks the measure off setpoint → the regulator
  spawns a scope → the scope does the work and closes → its fact returns the
  measure to setpoint → quiescence again. An unbounded cone never exists, even for
  an immortal daemon.

This **generalises the existing `resolver`** (`task.new → request.received`): the
resolver is a degenerate regulator ("on an external trigger, spawn a request").
Homeostasis adds *proactive* regulators that watch a view and spawn on their own
(memory consolidation, periodic reflection, retries).

- 💡 **Termination is fractal.** A scope's "done" = it closed on a terminal state.
  The system's "done" = it is at setpoint. Same predicate-over-state at both
  levels.
- 💡 **"Everything executes in a scope" sharpened:** *all WORK* executes in a
  scope. The top level is a thin regulating membrane (regulators + setpoint views)
  that does no work itself, so it needs no scope.

---

## 7. Composition modes: nested vs detached

Two ways one scope opens another — name them explicitly:

- **Nested = synchronous call (await).** The opener's cone stays open until the
  child closes; the child's closure is **sealed into the parent**, so a parent
  reaction picks up the output. Used for: phases a parent awaits, fan-out + join
  (N parallel children, parent quiesces only when all close).
- **Detached = asynchronous spawn.** The child is a fresh top-level cone
  (inherits no parent — already in code as `Detached`,
  [31](./31-meta-agent-and-root-scopes.md)); the opener does **not** block on it.
  Its closure lands at the **top level (unscoped)**, where a **regulator** picks it
  up. Used for: long-lived services, the architect's worker, background memory work.

**Addressing on spawn and return is uniform — it is all events:**
- spawn = emit the root kind with input as payload;
- return → next = a reaction on the closure that emits the *next* scope's root with
  the output as payload. Nested: the connector lives in the parent. Detached: the
  connector is a top-level regulator. *The output of a detached scope is the input
  of a new scope* — same connector, just routed through the membrane.

This connector *is* the router of §8.

---

## 8. Cycles as chains of bounded instances (the router)

A long-running loop must **not** be one unbounded cone. It is a **chain of finite
scope instances**: instance #N's output seeds instance #N+1's input. Each step is
bounded (its own budget), checkpointed (its closure is durable state), and
observable (a lifecycle event). This is tail recursion: each iteration is a fresh
"stack frame" (scope instance), not one growing frame.

**The router** is the reaction on the closure that reads a **continue/done
discriminant** and branches — the `if continue: recurse; else: return` of the loop:

```
scope step: root=step.start, budget={...}          # one BOUNDED iteration
# its body works; on close it exports an output that is one of:
#   {status:"continue", state:{...}}   or   {status:"done", result:{...}}

router on scope.step.closed:                        # ← a declarable built-in
  output.status=="continue" → emit step.start(payload=output.state)    # next iteration
  output.status=="done"     → emit step.result(payload=output.result)  # leave the chain
```

This is `while not done: state = step(state)`. The loop variable is the
carried-forward output; the condition is the discriminant; the router is the loop
control. The chain is finite-but-unbounded (ends when some instance emits `done`);
an enclosing scope may budget `step.start` to cap the number of iterations.

- 💡 **Already embryonic:** `TestDrain_StallReDrivesIntoNewChildCone`
  ([30](./30-scopes-state-and-fan-out.md)) already does the *continue* branch
  (re-drive opens a new child cone on `scope.closed`). What's missing: the *done*
  branch as a first-class discriminant, and making the router a **declaration**
  rather than a hand-wired bridge ([30](./30-scopes-state-and-fan-out.md) P3, the
  "promoter should be a built-in").

---

## 9. The agent loop, expressed as a STATE MACHINE (the chosen form)

> Supersedes an earlier "turn-scope per turn + ticker + router" sketch. For the
> *within-agent* loop, the per-turn child scope is **not needed** — the join is a
> state predicate. The turn-scope/router shape of §8 is for *between-phase* chains
> and for isolated fan-out (§9b), not for the tool loop.

**State is the interface; the scope is the hidden mechanism.** You design the
agent as a state machine and subscribe to *state* events; the scope underneath
only provides isolation + a budget backstop.

### The state (a projection `agent`, one per task scope)

```
{ task:       <issue text>           # set once, from task.new
  requested:  { id … }               # request events emitted, by id
  responses:  { id … }               # responses received, by the request id they answer
  status:     DERIVED:
              done        — a message was produced (explicit terminal mark)
              waiting     — requested ⊆ responses   (nothing outstanding → the LLM's turn)
              processing  — otherwise               (awaiting responses) }
```

`status` is **value-in-subject**: the view emits `state.status.<value>` only on a
*change* (CDC — "subscribing to a projection change is subscribing to a
`state_changed` event"). The value rides in the kind, so dispatch stays
payload-blind. (Implementable as a passive projection + a `derive-status`
reaction, so projections need not become active.)

### The reactions (everything written by hand)

```
init    on task.new                 → state.updated.task{text}      # opens the task scope, seeds state
llm     on state.status.waiting,    → tool.*.call …   OR   llm.message
        reads agent
hands   on tool.*.call              → tool.*.result | tool.*.failed
finish  on llm.message              → state.updated.status{done}    # terminal mark
```

`requested` / `responses` are **not reactions** — they are the projection's own
fold (the projection *is* the bookkeeping: what was asked, what came back, collapse
into `status`).

### Trace

```
task.new{"fix the bug"}
  agent={requested:∅,responses:∅} ⇒ status waiting → state.status.waiting
    llm fires, reads agent → emits read, search, test  (3 calls)
      agent.requested={r,s,t} ⇒ processing → state.status.processing
  hands answer (one at a time, single-writer log):
    read.result   → responses={r}     ⇒ processing
    search.result → responses={r,s}   ⇒ processing
    test.result   → responses={r,s,t} ⇒ status waiting (all in!) → state.status.waiting
      llm fires AGAIN, reads agent (3 results folded in) → emits edit
        requested={…,e} ⇒ processing
  edit.result → responses covers ⇒ waiting → state.status.waiting
      llm fires → emits llm.message "fixed"   (no calls)
        requested unchanged, still ⊆ responses ⇒ status STAYS waiting (no transition → no re-drive)
        finish on llm.message → state.updated.status{done} → terminal
```

### Why this is the chosen form

- ✅ **The LLM subscribes to nothing but `state.status.waiting`** — not to itself,
  not to tool results, not to a scope closure. One trigger.
- ✅ **The join ("all results in") is a state predicate** (`requested ⊆ responses`),
  computed by the projection. **No turn scope, no ticker, no router** — and the
  §11 node-vs-kind-rooting fork **dissolves** (there is no turn scope to root).
- ✅ **One LLM firing per turn, automatically:** it fires once per `→ waiting`
  transition; a turn's N parallel calls flip to `processing` once, N responses flip
  back to `waiting` once. This is the multi-call / lost-`thought_signature` problem
  solved *structurally* — without the per-call re-drive that caused it.
- ✅ **Done is the absence of a transition:** a message turn adds no calls → no
  `processing → waiting` cycle → no re-drive; `finish` marks done. No loop risk.
- ✅ **Expressible in today's primitives** — projection (passive fold) + reactions +
  value-in-subject. The only engine addition is emitting `state.status.<value>` on
  a derived-field change (a `derive-status` reaction; §10).

### 9a. The two layers

- **System level:** scopes and scope-lifecycle events (`scope.X.opened`,
  `scope.X.closed`). You *may* subscribe to these — it is legitimate, but it is
  plumbing.
- **Interface (programmatic) level:** you declare a scope (e.g. `task`) with a
  *state* (optionally a schema — it can be left unschematised), and you subscribe to
  **state events** (`state.status.waiting`, `state.updated.*`). This is the surface
  an agent author works on; the scope mechanism stays out of sight.

### 9b. A tool can BE a scope

A *simple* tool is an ordinary reaction (`tool.X.call → tool.X.result`), managed
in the same scope purely through the projection (above). A *complex* tool **opens
its own scope**: the call is the scope's input, it does whatever inside (its own
loop, its own budget, its own state), and it yields the result through its
**closure** (output). This unifies "a tool" and "a scope": input = the call,
output = the result. Simple ⇒ reaction; complex ⇒ scope — same `tool.X.call →
tool.X.result` contract from the caller's view (the caller cannot tell which).

### 9d. The View-reducer contract (the developer interface)

A **View is a stateful reducer over its scope's events** — the developer's primary
programming surface (code, not YAML). It unifies the View face (its cached state)
and the Reaction face (its emits) into one object:

```go
// State is held by the ENGINE per scope instance (a cache, rebuildable from the
// log by replay — G8). The body stays pure: state in, state + emits out.
type Reducer interface {
    Reduce(state any, ev Event) (next any, emits []Emit)   // fold one event, maybe emit (CDC)
}
```

The whole agent loop is one reducer (`Agent{task, requested, responses, status}`):
each event folds in; a `status` transition emits `state.status.<value>`. ~15 lines
of code carry the join, the loop, and the done condition (§9 trace).

**The graph never reads the code — it reads the declaration.** This is the
standardisation that keeps the topology from breaking, and it is exactly how the
`llm` body already works (arbitrary code, *declared* `Emits` allowlist; the engine
lints `emit ⊆ Emits` at runtime, CONCEPT §A.4). A View is therefore a normal
`Subscriber`:

```yaml
- name: agent.state
  in:    task                                      # attached to the scope (≥1 per scope allowed)
  on:    [request.received, tool.>, llm.message]   # what it reduces  → graph reachability
  emits: [state.status.waiting, state.status.processing, state.status.done]   # what it MAY emit → graph
  body:  { kind: view, config: { type: agent } }   # the reducer, resolved by name from the registry
```

Invariants that keep the topology valid:
- **Emits are declared, never introspected.** The transition kinds are a finite
  enum of registered `state.status.*` kinds; the reducer picks one at runtime,
  bounded by `Emits`. The graph sees the whole enum.
- **The body is named by a descriptor** (`BodyKind: view`, `BodyConfig`), like
  `llm`/tools — descriptor on the log, code in the registry, so G8 holds (state
  rebuilt by replay, wiring rebuilt from the fact).
- **A View is a `Subscriber` with a stateful body.** Scope attach = `In`; the
  topology language is unchanged — only a new body kind. Wiring is static (the
  graph); logic is code (the reducer).

This is the **division of labour**: a *library* of default nodes (`llm`, tool
plugins) you wire; *your* code is the View — how you aggregate events into state
and when you emit. YAML assembles reusable topologies; code models state.

### 9c. When you DO still need a sub-scope

The state-predicate join is for a *lightweight* gather (tool results in the same
scope). **Heavyweight fan-out** — parallel sub-agents, each needing its own
isolated budget/history — still uses a scope closure barrier (§7 nested + §3
output). The choice: parallel branches need **isolation/budget** → separate scopes
+ barrier; just **collecting answers** → a state predicate. Same "await all", two
weight classes.

---

## 10. What the engine needs (not built yet)

1. **Split `membership` into containment vs delivery.** Keep ancestor membership
   for **obligations/budget** (`enter`/`leave`/`tallyKind` — the parent must stay
   open and may cap nested work). Gate **delivery** so a reaction `in: S` (S ≠
   top-level) receives an event only if S is the event's *own* scope, not merely an
   ancestor — the encapsulation boundary. (`engine/scope.go` `deliver`.)
2. **Fix P1 — `scopeState` narrowest attribution.** Attribute a `state.updated.*`
   write to the writer's *narrowest* covering instance only, not every ancestor, so
   per-scope state is isolated and the closure snapshot is correct.
   (`engine/projection.go` `scopeState`.) Regression test = the
   [30](./30-scopes-state-and-fan-out.md) §1 fan-out example.
3. **Aspects (`in: *`).** Materialise an aspect reaction scope-local into every
   scope instance at open time.
4. **The router/promoter as a declarable built-in** (§8) — continue/done branch on
   a closure.
5. **`scope.X.opened` lifecycle event** (§ scope management) — currently only
   `closed` / `budget_exhausted` exist; opening should be observable too.
6. **Output = a declarable exported view, snapshotted at close** (§3) — generalise
   the closure payload beyond the default per-scope kv.
7. **The agent-state machine (§9):** a projection that folds `requested` /
   `responses` and derives `status`, plus a `derive-status` reaction that emits
   `state.status.<value>` on a *change* (value-in-subject CDC). This is what the
   *first SWE-bench experiment* exercises — the smallest agent, no phases.

---

## 11. Open questions / forks

- ✅ **turn-scope rooting — DISSOLVED.** The §9 state-machine has no turn scope, so
  the node-vs-kind-rooting fork is moot for the agent loop. Separately, **P2 is
  settled toward kind-rooted only** (spawn a scope = emit its root kind; the
  homeostat/router/opener all spawn by emit); `Subscriber.Scope` (node-rooting)
  should be removed. node-rooting's "trigger inside the cone" is not needed — the
  input rides on the root event's payload (§3).
- ⚠️ **Output = exported view** (§3) — accepted as the working default; revisit if
  a counter-example needs a richer output than a view snapshot.
- ❓ **Aspect exclusions** (§5) — negative/match pattern on the `*` axis (e.g.
  "every scope but `memory`"). Syntax.
- ❓ **Encapsulation as default and its blast radius.** Decided: encapsulation is
  *always on* (we are still in PoC — these runs only surface the holes, nothing
  ships to break). Consequence to work through: the current **architect** relies on
  a parent node (`bridge in: architect`) observing a child's emits (`brain in:
  compose`) — child→parent push, which encapsulation forbids; the architect must be
  restructured to read the draft via the compose **closure** (or a cross-scope
  view) instead. **coding-agent** similarly: `terminal-on-done` should listen on the
  request scope's *closure*, not its internal `state.updated.status`. These are
  principled restructurings, not regressions.
- ❓ **Memory write path.** Confirmed shape: producing memory is *execution* (a
  normal scope: llm calls, cross-scope view reads, emitting `memory.*` facts);
  retrieving memory is a *view* keyed by some keys — **not** a scope. There is **no
  identity/horizon scope** ([30](./30-scopes-state-and-fan-out.md) P4 is resolved
  this way, not by building a second scope species). Open: the keying/horizon
  convention for memory views, and whether a memory view should fold only facts
  from *closed* producing scopes.

---

## 12. Experiment 1 — VALIDATED on SWE-bench (psf/requests-3362)

The §9 state machine, built (engine `Reducer` + `nodes/view` + `topologies/state-agent.yaml`)
and run on the **real** instance the architect's worker had FAILED (doc 32: it
edited only tests). Brain: `vertex:gemini-3.1-pro-preview`. Result:

- ✅ **RESOLVED** — official grader: F2P `test_response_decode_unicode` **1 passed**,
  P2P **75 passed** (no regression). The diff is **source-only** (`requests/utils.py`
  `stream_decode_response_unicode`: when `r.encoding is None`, default to utf-8 +
  an incremental decoder instead of yielding raw bytes — the exact root cause).
- ✅ **The state machine drove every turn.** The llm subscribes ONLY to
  `state.status.waiting`; it fired **47** times = 47 `state.status.waiting` emits,
  whose sole source is the `agent.state` reducer. Spine cycled clean:
  `waiting 47 / processing 46 / done 1`, `llm.empty 0`, `request.terminal 1`.
- 46 real tool actions (12 edit, 11 read, 6 search, 3 write, 14 test); ~1.04M in /
  13.5k thought / 4.4k out tokens (~$1.4).

What this validates: **state-as-interface works.** The whole agent loop is one
~60-line `Reduce` (the join as a `Pending==0` predicate, the re-drive as a status
transition, the done condition as a no-tool turn) wired around library `llm` + tool
nodes; the scope is invisible plumbing (per-task state isolation + the budget
backstop). Sequential tool use kept the predicate exact under depth-first dispatch
(§9c); the parallel-batch gather (obligation counting) is the next refinement.

## See also

- [`CONCEPT.md`](./CONCEPT.md) — §2–§3 to be rewritten against this doc once built.
- [30 — scopes, state, fan-out](./30-scopes-state-and-fan-out.md) — P1 (fix in §10.2),
  P2 (the §11 turn-rooting fork), P3 (the §8 router), P4 (resolved in §11: memory is
  a view, not a scope), P5 (resolved in §1: scope is a primitive; its state is a view).
- [31 — meta-agent and root scopes](./31-meta-agent-and-root-scopes.md) — `Detached`
  (the §7 spawn mode), and the architect that §11 must restructure.
- [32 — architect e2e debug log](./32-architect-e2e-debug-log.md) — the runs whose
  leaks motivated this model.
