# 30 — Scopes, per-scope state, and parallel fan-out/fan-in (model + open problems)

> **Status: DESIGN / working notes.** Captures the scope + per-scope-state model
> as it stands, the parallel-exploration pattern (fan-out → per-scope state →
> promotion through closure), the recurring *subscriber roles* the model needs,
> and the concrete problems found while working a real example. Builds on
> [`CONCEPT.md`](./CONCEPT.md) §3 (scope is a built-in projection), §9 (the
> state-defined agent), and [26 §2a](./26-bare-substrate.md) (one state per
> scope). When this doc and `CONCEPT.md` disagree on the settled model,
> `CONCEPT.md` wins; the **Open problems** here are not yet settled.

---

## 1. The worked example — parallel exploration

Three subscribers explore a codebase from different angles in parallel, and
their findings are reconciled into the task state. This is the smallest topology
that exercises **fan-out, a barrier, per-scope state isolation, and promotion
through closure** at once.

```go
// events (always-on catalog: every domain kind is registered)
EventKind{Kind:"task.new"}                          // external entry
EventKind{Kind:"request.received"}
EventKind{Kind:"explore.start"}                     // roots the explore cone
EventKind{Kind:"state.updated.context.architecture"}
EventKind{Kind:"state.updated.context.feature"}
EventKind{Kind:"state.updated.context.memory"}
EventKind{Kind:"state.updated.context"}             // the merged result
EventKind{Kind:"state.updated.plan"}
EventKind{Kind:"state.updated.status"}
// scope.request.closed / scope.explore.closed are engine-owned (auto-registered)

// scopes (kind-rooted; see Open problem P2 — this is the proposed single knob)
Scope{Name:"request", Root:"request.received", Budget:{"explore.start":1}}
Scope{Name:"explore", Root:"explore.start",    Budget:{"tool.fs.read.call":64}}

// subscribers
Subscriber{Name:"resolver", On:["task.new"],         In:"global",  Emits:["request.received"]}
Subscriber{Name:"brain",    On:["request.received"], In:"request", Reads:["task_context"], Emits:["explore.start"]}

Subscriber{Name:"explore-architecture", On:["explore.start"], In:"explore", Emits:["state.updated.context.architecture"]}
Subscriber{Name:"explore-feature",      On:["explore.start"], In:"explore", Emits:["state.updated.context.feature"]}
Subscriber{Name:"explore-memory",       On:["explore.start"], In:"explore", Emits:["state.updated.context.memory"]}

// promoter: merge the explore snapshot into the parent (task) state — see §4
Subscriber{Name:"promote-context", On:["scope.explore.closed"], In:"request", Emits:["state.updated.context"]}

// planner: an LLM bridge that READS the merged context and writes a plan
Subscriber{Name:"planner", On:["state.updated.context"], In:"request", Reads:["task_context"],
           Emits:["state.updated.plan", "state.updated.status"]}

Projection{Name:"task_context", On:["state.updated.context", "state.updated.context.>"],
           In:HorizonRequest, Type:TypeKV}
```

Trace:

```
task.new                              ── external; resolver fires
 └ request.received                   ── opens `request` cone; brain fires
    └ explore.start                   ── opens `explore` cone (brain emitted it)
       ├ explore-architecture → state.updated.context.architecture{...}  ┐ 3 parallel cones, each
       ├ explore-feature      → state.updated.context.feature{...}       │ writing a DISTINCT path
       └ explore-memory       → state.updated.context.memory{...}        ┘ into the EXPLORE state
       explore cone quiesces when all 3 drain
    scope.explore.closed{ state:{                       ← join + merge, fired ONCE, in the
        "context.architecture":{...},                      PARENT (request) cone
        "context.feature":{...},
        "context.memory":{...} } }
    └ promote-context → state.updated.context{ merged } ← promotes ONE key into the request/task state
       └ planner → state.updated.plan + status{planning} ← waited on the closure by construction
```

The closure event is **`scope.explore.closed`** — named by the scope's *Name*
(`explore`), never by its root kind. It carries the cone's final state snapshot.

---

## 2. Per-scope state — the rule, stated precisely

**One state per scope instance** ([26 §2a](./26-bare-substrate.md)): the built-in
fold of the `state.updated.{path}` events *in that cone* into a kv `path →
payload`, born at the root event, frozen at close. Per-instance ⇒ isolation.

Applied to the example, the *intended* behaviour:

| scope instance | its state (kv) |
|---|---|
| `explore@…` | `{ context.architecture, context.feature, context.memory }` — **only its 3 paths** |
| `request@…` | `{ context (the merged value), plan, status, … }` — **not** the 3 sub-paths |

Two laws make this work:

1. **Writes are local.** A subscriber's `state.updated.*` write lands in *its
   own* scope's state (its `In` scope), not in any ancestor. The explorers write
   the `explore` state; nothing of theirs appears in the `task` state directly.
2. **Promotion is through closure only.** The closing scope carries its final
   state in `scope.{name}.closed`; a parent-scope consumer folds chosen fields
   up. This is the *only* `request ← explore` path. Live cross-scope writes are
   impossible by geometry — a mid-cone write is trapped in the cone.

So your reading is exactly right: **inside `explore` there are three vars; on
close we emit a single `context` into the task scope.** (The engine does not yet
*enforce* law 1 for nested state-writing sub-scopes — see **P1**.)

---

## 3. Fan-out / fan-in without a wait-group

Three mechanics, none of which the operator writes by hand:

- **Fan-out** = one trigger, many cones. Either N subscribers on the same kind
  (static, as above) or one firing that emits N events (dynamic — the `llm` body
  emits all its function-calls this way, `CONCEPT` §2).
- **The barrier = the scope closure.** The `explore` cone holds one obligation
  per explorer; obligation counting fires `scope.explore.closed` **exactly once**,
  when the last explorer drains (`CONCEPT` §3). You root a scope; you get a join.
- **The merge = the per-scope kv fold.** Distinct paths fold with no collision;
  the closure carries the merged snapshot. A **promoter** lifts it into the
  parent. The downstream `planner` fires on the promoted write, so it
  *structurally waits* for exploration to finish.

Variants:

- **Flat (no explore scope):** explorers write distinct paths straight into the
  `task` state (`In:request`). The fold reconciles; no promoter needed. You lose
  the explicit "exploration done" barrier and a separate exploration budget.
- **Per-explorer isolation:** each explorer gets its own sub-scope (same path, or
  independent budgets). N closures; the promoter joins all N. Needed when angles
  collide on a path or need isolated budgets.

The choice is exactly: **do the explorers share one state or own isolated
states?** Share → one cone, distinct paths, the fold merges. Isolate → a sub-cone
each, the promoter joins their closures.

---

## 4. Subscriber roles the model needs ("primitives")

The substrate has three primitives (Event, Reaction, Projection); a *subscriber*
is a Reaction wired with `On`/`In`/`Reads`/`Emits`. But a handful of **recurring
subscriber shapes** carry the whole model. Naming them clarifies which are domain
logic (an operator writes the body) and which are pure mechanism (candidates for a
**built-in declarable role**, no hand-written body).

| role | shape | body? | built-in candidate? |
|---|---|---|---|
| **entry / resolver** | external event → a domain root event (roots a scope) | operator | no — domain mapping |
| **bridge (llm)** | `Reads` a view → calls the model → emits allowlisted events | the `llm` body | no — the reasoning core |
| **effect / tool** | `On` a `*.call` → does a side-effect → emits `*.result`/`*.failed` | plugin | no — domain effect |
| **verifier** | re-runs a check → writes a *verified* state transition (the only writer of a terminal status) | operator | partially |
| **promoter / reconciler** | `On scope.X.closed` → folds chosen snapshot fields into the parent state | operator today | **YES** — see below |
| **terminal / lifecycle sink** | `On` a terminal state / `scope.X.closed` → emits the terminal fact, runs audit (consumes the closure so it cannot stall) | sink (no body) | partially |
| *projector* | *(not a subscriber — a declared `Projection`, a fold/view)* | — | — |

### 4a. The promoter should be a built-in

"Merge a closing sub-scope's state into the parent state" is **pure, mechanical,
payload-blind, and recurring** — exactly the kind of thing the engine should
provide declaratively instead of forcing a hand-written Reaction. Sketch of a
built-in form:

```go
// declare, don't hand-code: "when explore closes, lift its context.* into the
// parent state under `context` (or one key per field)."
Promote{ From:"explore", Into:"request",
         Map: { "context": "context.*" } }   // fold the matched sub-paths → one parent key
```

This is the engine doing the same fold it already does at closure, then writing a
`state.updated.{into}` in the parent cone — no operator body, no allowlist to get
wrong. Open question: does the merge ever need *judgment* (dedup, rank,
synthesize)? When it does, the promoter is genuinely an **LLM bridge**, not a
mechanical fold — so the built-in covers the mechanical case and the operator
drops to a bridge when synthesis is required.

### 4b. The relay anti-pattern

A subscriber that only **renames** an event (`On:[a]`, `Emits:[b]`, no `Reads`, no
effect) is almost always a *missing body*. In an earlier draft the `planner` was
`On:[state.updated.context] → Emits:[plan.requested]` — a pure relay that added
nothing the `status` spine didn't already carry. The real planner **reads** the
context and **produces** a plan. Rule of thumb: if a subscriber neither reads a
view, calls a model, performs an effect, nor folds a closure, ask what it is
*for* — it is probably a phase-marker the state spine already encodes.

---

## 5. Open problems (discovered working this model)

### P1 — Per-scope state attribution leaks across nesting *(bug)*

`scopeState` attributes a `state.updated.*` write to **every covering instance it
is a member of** (`engine/projection.go` `scopeState`), and an event's membership
includes **all** nested cones (`explore ⊂ request ⊂ global`). So an explorer's
`state.updated.context.architecture` is a member of both `explore@…` *and*
`request@…` — it **leaks directly into the task state** instead of staying
trapped in the explore cone until the promoter lifts it. The code admits the gap:
the intended rule is *"the writer's **narrowest** covering `In`-scope instance,"*
approximated as *"member of this instance"* — exact only when there is **no
nested state-writing sub-scope**. The §1 example is precisely that case.

**Fix:** attribute a write to the writer's narrowest covering scope instance (the
writer's `In` scope), not to every ancestor. Then law 1 of §2 actually holds and
isolation is enforced. The §1 topology is the regression test.

### P2 — Two rooting knobs for one concept, not even equivalent *(design)*

A scope can be opened two ways: **kind-rooted** (`Scope{Root:"k"}`) or
**node-rooted** (`Subscriber{Scope:"x"}` + a same-named `Scope` for the budget).
They are duplicative *and* subtly different — they root **different cone
boundaries** (node-rooted includes the trigger event; kind-rooted starts at the
emit), and node-rooted cannot carry a budget, so the scope name is written twice.

**Fix:** collapse to **kind-rooted only**. A `Scope` decl owns `{Name, Root
(required kind pattern), Budget}`; drop `Subscriber.Scope`. A subscriber opens a
scope the way it does everything else — by **emitting the root kind**. One mental
model, budget co-located with the rooting, an explicit named boundary, a simpler
validator (one rooting source). Cost is low: in the codebase only the example
`resolver` uses node-rooting; the scope tests are already kind-rooted.

### P3 — The promoter is hand-wired boilerplate *(design)*

"Merge sub-scope state into parent state" is mechanical and recurring but today is
a hand-written Reaction. Make it a **declarable built-in** (§4a). Reduces the
surface where an operator can mis-wire an allowlist or a fold.

### P4 — Causal scopes vs identity scopes are conflated *(model gap)*

Two different things wear the word "scope":

- **Causal scope (cone):** rooted by an event, membership by `caused_by`, has
  obligation-counting closure + per-instance frozen state + a per-cone budget.
  `task`, `request`, `loop`, `explore`. **Well built.**
- **Identity scope (horizon):** a group of causally-**independent** cones sharing
  a key — `project`, `session`, `tenant`, and degenerately `global`. Causality
  cannot link them (two tasks are two separate causal trees), so membership is
  **by a key carried in the trace**, not by a cone. It has **no causal closure**.
  **Half-built** — `HorizonSession` is declared but used by no topology; the old
  `app.session` subject class that carried the key was removed as a leak, leaving
  no clean key-on-trace mechanism.

**Direction:** keep causal scopes as is; build identity scopes on the **trace**.
Generalise the existing `request_id` (already "the nearest request instance's key,
in the trace") into a map `scope-name → instance-key` per event. A causal scope's
key = the root span (from `caused_by`); an identity scope's key = an attribute
stamped at the boundary (e.g. `project_id`) that propagates down the trace
unchanged. `global` = a constant key. Identity scopes feed projection horizons and
running budgets; they do not close. The id lives in the trace, never in the
subject (the `app.ingress`/`app.session` lesson: the subject is the event's name,
structure lives in the trace and the validated topology).

### P5 — `Scope` is its own `Decl`, but conceptually "a built-in projection" *(consistency)*

`CONCEPT` §3 says a scope *is* a built-in projection, yet in code `Scope` is a
distinct `Decl` separate from `Projection`. Defensible (the cone needs
obligation-counting the generic fold lacks), but worth revisiting whether a scope
is a `Projection` with a built-in `Type` (`cone`), so the substrate has fewer
managed-object kinds. Ties into P4: an *identity* scope really is just a
projection horizon; a *causal* scope is the cone fold.

---

## See also

- [`CONCEPT.md`](./CONCEPT.md) — §3 (scope as a built-in projection), §9 (the
  state-defined agent), §6 (the catalog).
- [26 — bare substrate](./26-bare-substrate.md) — §2a one-state-per-scope, §3
  termination as a scope budget.
- [27 — state-defined agent](./27-state-defined-agent.md) — the subscriber list.
- [29 — SWE-bench target](./29-swebench-target.md) — where this model gets
  exercised for real (the coding-agent topology, Iterations 4–5).
