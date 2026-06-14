# 27 — State-defined agent: compile a reconciler to subscribers, let the engine validate connectivity (EXPERIMENT)

> **Status: EXPERIMENT / first bootstrap step on the converged substrate.**
> Captures a design session building a simple coding agent as a *state
> model*, compiling it to a flat *list of subscribers*, and having the
> **engine** validate graph connectivity. Spends [24](./24-concept.md) and
> [26](./26-bare-substrate.md); adds no primitive. The deliverable of step 1
> is **not a running agent** — it is: *define the subscriber list, and run
> connectivity validation through the engine.* The engine, not a human,
> decides "connected / has gaps".

---

## 1. The programming model: you define state + subscribers, the engine validates

The human writes two things and nothing else:

1. **State** — the phases of a task and the transitions between them (the
   reconciler's shape; §2).
2. **Subscribers** — a flat list of `node + on(event) + in(scope)` (§3).

The engine compiles the subscribers' `emit → subscribe` relation into a
graph and **validates it**: either the graph is connected (every demand has
a producer, every product a consumer, every cycle bounded) or it has
**gaps**. At a gap the engine *suggests an LLM bridge* — "this kind is
emitted by nobody; add an LLM node that can emit it" — you add the one
subscriber, the engine re-validates. The point of the whole model:

> You never hold the entire graph in your head. You add one subscriber; the
> engine tells you whether it breaks connectivity. Local edit, global check.

**LLM nodes are the bridges** ([24 §1](./24-concept.md), [26 §4](./26-bare-substrate.md)):
they sit at dead-ends (events with no consumer) and emit allowlisted events
that are the entry points of otherwise-disconnected fragments. There is no
"brain": there are many LLM nodes, each a bridge over one gap, each with its
own subscription and config (model, prompt, **emit allowlist**) — one
universal `llm` body parameterised by the log ([24 §A.4](./24-concept.md)).

## 2. The state model (the worked example)

A simple coding agent. The state is a record; the human thinks in it
directly (the reconciliation layer — desired = "task done", current =
`task_state`, the agent is the reconciler):

```
task_state {
  task:    "write feature X"
  understanding {
    goal:        "…what to do…"
    context:     [ { asked, found }, … ]
    sufficiency: insufficient | sufficient
  }
  plan:    [ { step, status: pending | done }, … ]
  status:  new | gathering | planning | executing | done | needs_clarification
  result:  ""
}
```

Transitions (`done`, `needs_clarification` are final):

| from | to | condition |
|---|---|---|
| `new` | `gathering` | task accepted |
| `gathering` | `gathering` | `sufficiency = insufficient`, sources remain |
| `gathering` | `planning` | `sufficiency = sufficient` |
| `gathering` | `needs_clarification` | `insufficient`, no sources left |
| `planning` | `executing` | plan written (≥1 step) |
| `executing` | `executing` | `pending` steps remain → take next, do it, mark `done` |
| `executing` | `done` | all steps `done` (and verified) |

Two self-looping states, each with an explicit exit gate — `sufficiency`
for `gathering`, "all steps done" for `executing`. "Done" is not a vague
confidence; it is **plan completion**, tracked per `plan[].status`.

## 3. Compiling state to subscribers

Compilation rule: a **state transition is an LLM node bridging a dead-end to
the entry of the next fragment**. Mechanics, decided this session:

- **Field updates live in the subject** — `state.updated.sufficiency`,
  `state.updated.plan` ([24 §2](./24-concept.md): state paths are subjects,
  subscription is a wildcard, not a payload filter). You subscribe to a
  *field's change*, not to a blob.
- **Values live in the payload**; the consuming reaction reads them (the
  engine is payload-blind, a reaction is domain code and may read).
- **A value becomes its own kind only for a deterministic fork** —
  `task.answered` vs `task.needs_clarification` are distinct kinds, not two
  values of one field. This avoids exploding the subject space.

The subscriber list (the two corrections from this session folded in:
**every node has a scope — minimum `global`, never none**; **`understand` is
read-only**):

| node | `on:` | `in:` | reads | emits (allowlist) |
|---|---|---|---|---|
| `resolver` | `app.ingress.*` | `global` | session binding | `request.received` (roots `request`) |
| `understand` (llm, **read-only**) | `request.received` | `request` | `task` | `state.updated.goal`, `state.updated.status`, **only** `tool.fs.read.call` / `tool.fs.search.call` |
| `gather` (llm) | `tool.fs.*.result` | `request` | `project_context` (global) + `task_context` (request) | `state.updated.context.found` (req), `sys.…project.context.found` (global), `state.updated.sufficiency`; then loop `tool.fs.read.call` **/** `plan.requested` **/** `task.needs_clarification` |
| `plan` (llm) | `plan.requested` | `request` | `goal`, context views | `state.updated.plan`, `state.updated.status{executing}` |
| `execute` (llm) | `state.updated.plan`, `tool.*.result` | `request` | `plan`, `task_context` | next step `tool.*.call`, `state.updated.plan.{i}.status`; when no `pending` → `state.updated.status{done}`, `task.answered` |
| `fs`, `gotool` (tool) | `tool.X.call` | `global` | — | `tool.X.result` / `.failed` |
| *(no guard node)* | — | — | — | the `gather` / `execute` loops are bounded by the **budget on their covering `request` scope** (§3a), not by a node |
| `notify` (sink) | `task.answered`, `task.needs_clarification` | `request` | — | reply to the user |

**Cycles.** A cycle is a node subscribing to the *result of work it itself
emitted*: `gather` emits `tool.fs.read.call`, the `fs` island answers
`tool.fs.read.result`, `gather` consumes it and either loops (emit another
read) or exits via a different kind (`plan.requested` — a dead-end where the
next bridge attaches). The SCC `{gather, fs}` is a real cycle and **must**
be covered by a scope that declares a budget ([26 §3a](./26-bare-substrate.md)) —
the validator enforces it. Determinism is irrelevant; the budget on the
covering scope is what bounds the loop.

## 4. Scopes — always one; the daemon model

**No node is scope-less. The minimum scope is `global`.** Three live in this
agent:

- **`global`** — daemon-wide, session-less (`sys.` facts, outside every
  cone). Holds: the shared **`project_context`**, **config** (system prompt,
  project orientation, per-node prompt/model config as facts —
  [24 §A.4](./24-concept.md)), and lifecycle events like **`agent.started`**.
- **`request`** — one per task. **N parallel tasks = N isolated `request`
  cones.** Holds `task_state`.
- **node-loop** — the `gather` / `execute` loops, within a `request`.

**Parallelism is not N agents — it is one node-set firing in N cones.**
There is one `gather` declaration; it fires inside each `request` cone
independently, and because its views are bound `in: request`, each firing
sees only its own task. Isolation is geometry (`caused_by`,
[24 §6](./24-concept.md)), not addressing. A background daemon running four
tasks at once is four `request` cones over one standing topology.

**Shared context, deliberately.** `gather` reads `project_context`
(`in: global`) first — if the project was already explored, `sufficiency`
can be `sufficient` with no re-reads. New findings it publishes *up* as
`sys.` facts (`sys.state.updated.project.context.found`) so the other three
tasks reuse them; task-private findings stay `request`-scoped. This is the
[25 §3](./25-regulation-concept.md) pattern (global state as `sys.` facts,
outside cones) applied to context instead of energy.

**Config arrives as global events.** `agent.started` seeds the global scope;
the system prompt, project orientation, and each LLM node's
prompt/model/allowlist are config facts a node `reads:` as a kv view — the
wiring/behaviour split of [24 §A.4](./24-concept.md). Re-prompting a node is
one event, not a redeploy.

## 5. Connectivity validation is the engine's job

The human does **not** eyeball the graph. The engine, run as a dry-run
(`reflex validate`, the changeset validator of [20](./20-topology-management.md)/[24 §7](./24-concept.md)),
folds the subscriber list into a graph and reports:

- **dead-ends** — a kind in some node's `Emits` that no node's `On`
  consumes (an unbridged gap → suggest an LLM node emitting the entry kinds
  of the disconnected fragment);
- **unreachable nodes** — a node whose `On` kinds are emitted by nobody and
  are not an ingress root;
- **disconnected fragments** — islands with no path from an ingress root;
- **unbounded cycles** — a non-trivial SCC not covered by a scope that
  declares a budget ([26 §3a](./26-bare-substrate.md)) → reject;
- **stalled closures** — `scope.X.closed` is an engine-emitted kind; a scope
  whose close can land on a non-terminal state needs a consumer of
  `scope.X.closed` (an LLM bridge or a deterministic terminator) or a
  provably terminal-only close — otherwise the cone can freeze in the void
  ([26 §3f](./26-bare-substrate.md)) → suggest a bridge;
- **allowlist lints** — a node emitting outside its declared `Emits`
  ([24 §A.4](./24-concept.md)).

The output is "connected" or a list of gaps with **LLM-bridge
suggestions**. Add the suggested subscriber, re-run, converge.

## 6. Gap analysis — `engine/` and `nodes/` today vs step 1

**What is already there (contracts, mostly skeleton):**

| Piece | File | State |
|---|---|---|
| `Event` / `Trace` / `Emit` (no terminal flag) | `engine/event.go` | defined |
| `Reaction` / `Views` / `KV` | `engine/reaction.go` | defined |
| `Decl`: `Node{On,In,Reads,Emits,Scope,Body}`, `Scope{Root,Budget}`, `Projection{On,In,Shape,Key,Value}`, `Horizon` incl. `global` | `engine/topology.go` | defined |
| `Engine{log,decls}`, `New`, `Events()` | `engine/engine.go` | `Events` implemented; rest skeleton |
| `tool.Node(name, fn)` + reaction | `nodes/tool/tool.go` | implemented |
| `llm.Config` / `llm.New` | `nodes/llm/llm.go` | `New` panics (skeleton) |

**What is missing for step 1** (define subscribers + engine-run validation):

| Need | Status | Note |
|---|---|---|
| `Engine.Apply` validation path | `panic` | must fold decls → live table and validate the resulting graph, **without** Append/Drain |
| a **connectivity validator** | absent | the §5 checks: dead-ends, unreachable, fragments, SCC budget-coverage, allowlist lint |
| **scope defaulting** | absent | `Node.In` empty → `global`; never scope-less (this session's correction) |
| ~~a node-type tag~~ | **superseded** | a `Node.Kind` tag was added here, then removed: determinism does not give termination, so the cycle check is scope-budget coverage, not a node attribute (doc 26 §3) |
| a **dry-run entry point** | absent | `reflex validate` returns the report without mutating |
| the **example topology** as decls | absent | the §3/§4 subscriber list, expressed as `[]Decl`, including the global config/`project_context` projections |

**Step 1 does NOT need:** `Append`, `Drain`, `llm.New`, any provider call.
It is a small, bounded milestone — *topology in, validation report out.*
The whole reconciler chain stays un-run.

## 7. The first step (defined, not yet built)

Implement, in order:

1. Scope-defaulting to `global`. (A `Node.Kind` tag was added here and then
   removed — see doc 26 §3; the cycle check is scope-budget coverage.)
2. `Engine.Apply` validation path: fold `[]Decl` → live table.
3. The connectivity validator (§5) over the live table.
4. A dry-run surface returning the report (connected / gaps + bridge
   suggestions).
5. The §3/§4 subscriber list as an example `[]Decl` (with the global
   `project_context` and config projections), exercised by a test that
   asserts the report — e.g. "remove `notify` → `task.answered` is a
   dead-end → engine suggests a bridge".

No event is appended; no drain runs. The acceptance check is: the engine,
not a human, says whether the subscriber list is connected, and names the
gaps when it is not.

## See also

- [24-concept.md](./24-concept.md) — three primitives, subject grammar (§2),
  config-as-facts (§A.4), changeset validation (§7), the standing test (§D).
- [26-bare-substrate.md](./26-bare-substrate.md) — scope as a built-in
  projection, termination as a scope budget (the cycle budget-coverage
  check, §3a), the LLM emits allowlisted events not tool-calls (§4).
- [25-regulation-concept.md](./25-regulation-concept.md) — global state as
  `sys.` facts outside cones (the `project_context` pattern).
- [20-topology-management.md](./20-topology-management.md) — the changeset
  validator this dry-run is the read-only mode of.
- [19-projections.md](./19-projections.md) — the kv/log views the nodes
  `read:`; `in:` horizons incl. global.
