# 29 — Target: one SWE-bench Lite instance on the new stack (TARGET / plan)

> **Status: TARGET / plan. LIVE.** The concrete north star for the next line of
> work: drive **one real SWE-bench Lite instance end-to-end on the new kernel**
> — a state-defined coding agent, a real model, completion by verification — and
> score it with the official harness. Builds strictly on [`CONCEPT.md`](./CONCEPT.md);
> the agent shape is [27](./27-state-defined-agent.md), the control plane is
> [`outdated/20`](./outdated/20-topology-management.md) re-founded on the new
> engine. This doc is the *what and why*; [28](./28-substrate-rebuild-roadmap.md)
> tracks engine-internal stages.

---

## 1. The target

Take one instance from `princeton-nlp/SWE-bench_Lite`, give our agent the repo at
`base_commit` plus the GitHub-issue text, and have it **produce a `git diff` that
resolves the issue** — verified by the official harness running the hidden
`FAIL_TO_PASS` / `PASS_TO_PASS` tests in the instance's Docker image.

The point is **not** the score. The point is to prove the new stack carries a
real coding task end-to-end: a daemon hosting the new engine, a topology applied
through the control plane, a real model reasoning in a loop, real
file/test events, and **completion as a verified state** — exactly the model in
`CONCEPT.md`, exercised for real instead of on a stub.

**Success, two tiers:**
- **First light (the real bar):** the agent, given the issue, loops
  read→edit→test on the checked-out repo, drives its task state to a *verified*
  `done`, and emits a coherent unified diff. End-to-end on the new stack, real
  model, no stub.
- **Resolved (the stretch):** the official `swebench.harness.run_evaluation`
  reports the instance **resolved**. Model-dependent (Gemini gives a low resolve
  rate; a strong model is an access question, not a code one) — first light does
  not hinge on it.

## 2. The boundary — framework vs benchmark glue

Settled and load-bearing: **nothing SWE-bench-specific goes inside the
framework.**

- **Framework (Go, committed, generic):** the new engine, the daemon + control
  plane + send-message/wait, the agent topology, the file/test subscriber nodes.
  None of it knows the word "SWE-bench".
- **Benchmark glue (throwaway bash, outside the framework):** `git checkout
  base_commit` (or just use the instance's Docker image, whose `/testbed` already
  is the repo + deps), `docker run`, feed `problem_statement` to the daemon,
  `git -C /testbed diff` → `predictions.jsonl`, run the official harness.

## 3. SWE-bench Lite mechanics (condensed)

300 instances over 11 Python repos. One instance =
`{instance_id, repo, base_commit, problem_statement, test_patch, FAIL_TO_PASS,
PASS_TO_PASS}`. The agent sees the repo at `base_commit` + the issue text; it
**never sees `test_patch`**. Output is one `predictions.jsonl` line:
`{instance_id, model_name_or_path, model_patch}`. The official harness applies
the patch then the hidden `test_patch` in the instance's Docker image and runs
the tests; **resolved** iff all `FAIL_TO_PASS` pass and no `PASS_TO_PASS`
regresses.

Settled environment decision: **run everything inside the official per-instance
Docker image** (its `/testbed` is the repo at `base_commit` with deps + a conda
env). Cross-compile the daemon + the agent for linux, `docker cp` in, run the
agent against `/testbed`, native `pytest`.

## 4. The agent — a state-defined topology (events + subscribers)

Per [27](./27-state-defined-agent.md) and `CONCEPT.md` §9. **No "tools", no
"menu"** — events and subscriber nodes. Completion is a **verified state**, not
an LLM claim and not quiescence.

### 4a. Task state (the `request` scope's one state)

```
task_state {
  task:    "<the GitHub issue text>"
  status:  working | verifying | done | failed     # terminal: done | failed
  notes:   "<agent scratch>"
}
```

`needs_clarification` is dropped — there is nobody to ask in a benchmark. The
spine is `status`; `done`/`failed` are terminal values.

### 4b. Subscribers (the flat list)

| node | `on:` | `in:` | emits (allowlist) |
|---|---|---|---|
| `resolver` | `task.new` (an external entry kind, registered, produced by none) | `global` | `request.received{task}` (roots `request`) |
| `brain` (llm, **loops**) | `request.received`, `tool.fs.*.result/failed`, `tool.py.test.result/failed`, `verify.failed`, `judge.rejected`, `llm.empty` | `request` | `tool.fs.read/edit/write/search.call`, `tool.py.test.call`, `llm.message`, `llm.empty`, `llm.usage` |
| `tool.fs.*.call` / `tool.py.test.call` (hands) | their own kind | `request` | the plugin's `*.result/failed` (the daemon backs each by the plugin that handles its kind) |
| `verifier` | `llm.message` (the prose claim) | `request` | re-runs the agreed test command; green → `check.passed`, red → `verify.failed` |
| `judge` (llm, independent) | `check.passed` | `request` | `judge.approved` (an affirmative call) / `judge.rejected` (its `Answer==Empty` fold — prose *or* silence) |
| `done` | `judge.approved` | `request` | `state.updated.status{done}` |
| `terminal` | `state.updated.status` | `request` | when terminal → `request.terminal` |
| `costs` (sink) | `llm.usage` | `request` | — |

- **The loop is event closure**, bounded by a `Budget` on the `request` scope —
  the termination backstop (`CONCEPT.md` §4). Every brain turn emits exactly one
  of the budgeted loop kinds (`tool.fs.*.call`, `tool.py.test.call`, `llm.message`,
  `llm.empty`), so capping them caps the loop. `brain` reads its `llm.history` view
  (`CONCEPT.md` §7); the issue text is the first user message, file/test results
  are the appended tail.
- **History replays as *structured* function calls, not text.** The `llm.history`
  view re-emits the model's own tool calls and their results as function-call /
  function-response parts (the function name is the event kind), not narrated prose
  — required for reliable multi-turn function calling on Gemini. A thinking model's
  per-call `thoughtSignature` rides on the call event via an engine-blind `Meta`
  channel (`engine.Emit`/`Event` carry an opaque `Meta` field) and is echoed back on
  replay, or the API rejects the request.
- **The brain never asserts done with a function. A turn that calls NO function
  IS its claim of completion** — prose surfaces as `llm.message`, empty/silence as
  `llm.empty`. The claim is then *evaluated*, never trusted: `llm.message` →
  `verifier` re-runs the tests deterministically (green → `check.passed`, red →
  `verify.failed` back into the loop); `check.passed` → `judge` (an independent
  llm node) reviews whether the change genuinely resolves the issue or merely games
  the tests, emitting `judge.approved` (an affirmative call) or `judge.rejected`
  (its `Answer==Empty` fold, so prose *or* silence both reject). Only
  `judge.approved` writes `state.updated.status{done}`; `judge.rejected` and
  `llm.empty` go back to the brain. **Done is a *verified* value — a deterministic
  check AND an llm reviewer**; green is necessary but not sufficient. The judge is
  *just another llm node* (a config, not a new abstraction) — exactly closure ≠
  completion (`CONCEPT.md` §3/§9).
- **`terminal` consumes the closure path** so no close stalls in the void (the
  stalled-closure validator check is satisfied).

### 4c. Verification caveat (honest)

The agent cannot run the hidden `FAIL_TO_PASS` (it never sees `test_patch`). The
`verifier` runs the **repo's own tests** / a target the agent reasons toward —
self-verification, not the harness's judgement. The official harness is the
external, final word (§2). So framework-`done` means "agent verified against what
it could see"; **resolved** is the harness's call.

## 5. What is already built (from `CONCEPT.md` §11)

The engine (append/drain, scopes/budgets/closure, projections + per-scope state,
catalog, view types), the `llm` body (real provider via `pkg/provider` →
allowlisted emits + `llm.usage`), `tool.Node`, and the real Vertex adapters
(Gemini works on `iow-uagent`; Claude blocked only by GCP access). All
`-race`-green. The agent's *reasoning core and its loop mechanics already exist*;
what is missing is the *runtime shell* and the *hands*.

## 6. The plan — framework gaps, in order

Proposed sequence (iterations, to confirm). The lean is **do it right**: the
daemon and real-time management stand on a log fold, not a slice.

**Iteration 1 — Control plane as changeset events on the new engine. ✅ DONE
(`645a0bc`).** Topology is now a **fold over `sys.topology.changeset.*` /
`sys.subscriber.registered` … facts** (`foldTopology(log, bodies)`), not the old
`Apply(decls)` slice. `Apply` is the in-process *client* emitting
`changeset.requested → facts + applied | rejected`; `Validate` is the
resulting-graph validator (now validating the cumulative graph). A node's `Body`
is the one non-serializable part — resolved by name from a process registry, the
same way view-type builders and provider adapters are code, not facts. The live
table round-trips as a fold of the log (G8). This is
[`outdated/20`](./outdated/20-topology-management.md) on the new engine; what
remains for "add a node in real time" is the **daemon/CLI/API surface**
(Iteration 2) that lets an *off-process* client submit ops (and bind a node
name → a registered body factory).

**Iteration 2 — Daemon around the new engine + send-message/wait. ✅ DONE
(`79fb036`, `5baf443`, `52b0201`).** Built in two parts: **2a** — serializable
body descriptors (`BodyKind`+`BodyConfig` on the log) + a factory registry
(`nodes.Register`/`Resolver`) + `engine.Load` so a topology (including which body
each node runs) is recoverable from the log; **2b** — a declarative topology
document (`pkg/topology`, YAML/JSON), a long-lived engine host (`pkg/daemon`)
with a unix-socket HTTP API (apply/validate/emit/topology/events), and the
`reflexd` CLI (`serve`/`apply FILE`/`emit --subject --wait`/`topology`/`validate`).
`emit --wait <kind>` is the send-message/drive-to-terminal verb; `apply` against
a running daemon is live topology config. **Deferred:** disk persistence of the
log (the daemon is in-memory; `Load` proves replay works), and the richer
`scopes`/`diff` read surface.

**Iteration 3 — The hands: everything is a plugin.** One uniform mechanism, no
special cases. The only true in-process body is `llm` (the reasoning core); every
*hand* (`fs.*`, `py.test`, `go.*`, …) is an **out-of-process stdio plugin**.

**3a — the plugin seam (stdio). ✅ DONE (`9716340`, `58bf01a`, `6efbb7d`).**
- a generic in-binary `"plugin"` body (`nodes/proxy`) — a proxy `Reaction` that,
  when its node fires, forwards the triggering event to a child process and
  returns the emits the child sends back;
- the wire (`pkg/plugin`, engine-free): NDJSON over the child's stdin/stdout —
  `hello`/`welcome` handshake, then per-firing `invoke{id,event}` →
  `result{id,emits[],error?}`;
- a small plugin SDK (the deleted `pkg/sdk` transport-adapter shape was the
  template) so a plugin author writes `plugin.Serve(spec, handler)`;
- the proxy is built so a **socket** transport drops in later (for plugins with
  an independent lifecycle); stdio is the simpler start.

**A plugin self-registers; it is NOT an operator-declared topology node.** Two
concerns, kept separate (operator correction, `b95f75a`→this iteration): a
*plugin* just exposes "I handle these kinds (with schemas), I emit these kinds
(with schemas)" and subscribes to handle them — full stop; the *node* concern
(who emits what, who subscribes to what) is the operator's graph wiring, a
separate layer. The daemon **launches** a plugin (`Daemon.LaunchPlugin`,
e.g. from `serve --root`), reads its `hello` self-description — `events: [{kind,
schema, role: in|out}]` — and turns that announcement into a **global subscriber
node** (`On` = the kinds it handles, `Emits` = the kinds it produces, body = a
`plugin` descriptor carrying the spawn command) **plus one `EventKind` per
declared kind+schema, in AND out**. The operator topology never writes a
`body_kind: plugin` node; it just emits the kinds a hand consumes and consumes
the kinds it produces. These plugin decls are folded into the next

> **SUPERSEDED by the §6 Iteration-4 finding (`50b7d55`) and its follow-up.** The
> auto-created **global subscriber node** described in this paragraph was the wrong
> call. The model now has two distinct concerns: a top-level **`plugins:`** section
> **registers the external process** (its spawn command + transport) — scope-agnostic
> infra; and **USING** a hand is a subscription the **operator declares** — a
> **normal subscriber named after the tool-call kind it serves** (e.g.
> `tool.fs.read.call`), scoped `in: request`, with **no plugin reference**. The
> daemon backs that subscriber with whichever registered plugin **handles its kind**
> — the link is the kind, not a name. So the operator owns the wiring and the scope,
> while the plugin still self-describes its kinds. Read the rest of this paragraph
> with that correction in mind.

The OLD (superseded) behaviour: these plugin decls are folded into the next
`apply`/`validate` alongside the operator document, so the **resulting graph is
validated as a whole** — a launched-but-unwired plugin is correctly a gap
(unreachable / dead-end), and connectivity is a property of the assembled graph,
never of a lone handler. The schemas come from the plugin and land on the log as
`sys.event.registered` facts; `reflexd` hardcodes nothing.

The subscriber's scope is **`global`** on purpose: a plugin is scope-agnostic —
it handles its kinds wherever they occur, and the engine places its emits in the
trigger's cone by causality (a reaction never chooses its emit's scope, doc 24
§5). `engine.Load` rebuilds the subscriber node from `sys.subscriber.registered` and
re-spawns the process from its descriptor without re-launching (G8).

Properties this preserves: the engine's **emit-allowlist still binds the plugin**
(out-of-process is not out-of-bounds); the body descriptor (`kind` + spawn
command) rides on `sys.subscriber.registered`, so the wiring is **recoverable from the
log** (G8); and **hot-plug** ("agent launches a plugin, applies a topology that
emits its kinds, uses it, no daemon restart") is a property of live `apply`, not
the transport.

**Plugins ship in-repo as a multi-call binary** — `reflexd plugin fs`,
`reflexd plugin pytest` are subcommands the daemon spawns as separate child
processes over stdio. Code lives in one binary ("built-in"), each plugin runs as
its own process ("launchable separately"). Composability falls out: the daemon
only launches the hands it is told to (`serve --root` brings up `fs`+`pytest`;
a non-filesystem agent simply launches neither). The protocol is
language-agnostic (a plugin can be native Python).

*Finding (3a) — RESOLVED (follow-up):* adopting the catalog used to flip on
**full catalog enforcement** (`validate.go`: a non-empty catalog gated
`Connected`), so the whole topology then had to declare every kind including
engine internals. That all-or-nothing dormancy is **gone**: validation is now
**always on** (registering an event is a first-class, required operation), and
**the engine self-registers the kinds it owns** through the same catalog — the
seed `event.registered` and, per declared scope, `scope.{name}.closed` /
`.budget_exhausted`. So the operator declares **only domain kinds**; a launched
plugin tripping enforcement is fine because engine internals carry themselves.
There is no "ingress kind" to auto-register — the `app.ingress` class was
removed (an external event is a plain registered domain event; a root is
structural). The `llm` body still gets a full catalog to advertise function
schemas — that was always the goal, now reached without boilerplate.

*Vocabulary (this iteration):* the substrate wiring unit is a **`Subscriber`**,
not a "node". The type, the fact (`sys.subscriber.registered`), the changeset
op-kind (`"subscriber"`), and the topology document key (`subscribers:`) were
renamed accordingly; "node"/"edge" now live **only** in the connectivity-graph
projection (the validator). A `Subscriber` is `On`/`In`/`Emits` + a `Reaction`
body; the graph "node" is its image in that projection. A plugin, on launch,
self-registers as one global `Subscriber`.

**3b — the hands themselves. ✅ DONE (`a2cfb62`, `8f2f610`, `54c6af8`).**
`fs.{read,edit,write,search}` (ported fs logic, root-confined, read-before-edit
guard) and `py.test` (shell out; exit 0/1 → result, else → failed) as
`reflexd plugin` subcommands on the SDK, each announcing its kinds + parameter
schemas in `hello`.

The `llm` body needs **no per-tool wiring** — a callable function already *is* a
kind in the node's `Emits` ("the menu", llm.go: no separate tool-menu concept).
The only §12 gap is that the advertised `ToolSchema` carries no `InputSchema`;
3b fills it from the **catalog** (which 3a populates dynamically from the
plugin). The engine already folds the catalog at dispatch (`catalogSchema`); 3b
exposes that lookup on the dispatch read-surface (a `Views.Schema(kind)`
accessor — no `Reads`, no wiring) and the `llm` body sets each function's
`InputSchema` from it. Adding a tool = register its kind in the llm node's
`Emits` + have the plugin announce the schema → the catalog carries it → the
`llm` advertises it. Schemas must be the LLM-tool-compatible subset.

A plugin's **root** (e.g. the fs sandbox dir) is a **host concern**: `reflexd`
determines it and passes it at launch (`reflexd plugin fs --root <dir>`, wired
from `serve --root`), never an operator topology config. The root confines the
process; it is not part of the graph the operator declares.

**Iteration 4 — The coding-agent topology + verification flow, run locally.
✅ DONE — the live Gemini drive is green.** §4b is now a committed topology document
([`topologies/coding-agent.yaml`](../topologies/coding-agent.yaml)) wiring the
real Gemini brain (`vertex:gemini-2.5-pro`) to the launched fs/pytest hands. What
shipped:

- **The control bodies** the §4b graph needed beyond `llm`: `entry` (the
  entry/resolver role — boundary kind → domain kind), `gate` (the terminal gate —
  drive `request.terminal` only when a state field is terminal), and `verifier`
  (the *deterministic* half of completion: on the brain's prose claim re-run the
  agreed check, green → `check.passed`, red → `verify.failed` back into the loop).
  Each is a registered `nodes.Factory`, unit-tested. The brain only *claims* done
  (a no-function turn); the verifier confirms the tests, and the `judge` — *just
  another `llm` node*, config not a new body — reviews the change and gates `done`.
- **A deterministic integration test** (`pkg/daemon/coding_loop_test.go`) drives
  the whole loop on the real engine with the model + hands replaced by stand-ins:
  the green path (prose claim → `check.passed` → `judge.approved` → verified done
  → `request.terminal`) and the red path (the check never passes → the loop
  re-claims until the `request` scope budget caps it → `budget_exhausted` → a
  failed terminal). Proves the loop, the
  verification flow, and the terminal drive without spend.
- **A real-hands apply test** (`coding_agent_topology_test.go`) launches the
  actual fs + pytest plugins, folds them with the document, and asserts the graph
  is connected and the `llm` body resolves its Gemini provider (lazily — no model
  call). The structural proof up to the live-model boundary.

*Finding (Iteration 4) — a plugin-model correction (`50b7d55`):* the first attempt
hit a connectivity-validator rejection — the agent loop `brain ↔ fs/pytest` read
as an unbounded cycle because the launched hands were **global** subscribers
outside the budgeted `request` scope. The first fix tightened the validator
(edge-kind boundedness) to tolerate that, but the **real** defect was upstream and
in the plugin model itself: `LaunchPlugin` **auto-created a global subscriber**,
force-wiring a hand into the graph with no operator say over its scope. Corrected
(operator): **launching a plugin exposes a capability; USING it is a subscription
the operator declares**, like any other — a **normal subscriber named after the
tool-call kind it serves** (e.g. `tool.fs.read.call`), with an operator-chosen
`In` and **no plugin reference**; the daemon backs it with whichever registered
plugin handles its kind (the link is the kind). The hands are now declared
`In: request` (inside the budgeted loop), so the loop is bounded the ordinary way
and the validator needed **no** change — the earlier edge-kind tightening was
reverted (it was patching a symptom). This reverses the auto-global node of
`faf9f64`: a top-level `plugins:` section registers the process; the operator owns
the wiring.

**The live Gemini drive — done, green.** Against a `serve --root <checkout>`
daemon (real `vertex:gemini-2.5-pro` on `iow-uagent`, real fs/pytest hands) a
synthetic task (a buggy `add` whose test fails) drove to a *verified* terminal:
`task.new → search → test(fail) → read → edit → test(pass) → llm.message(claim)
→ check.passed → judge.approved → done → request.terminal`. The verifier re-ran
the suite green and the judge affirmed the change. The brain produced the correct
one-line fix; an independent `pytest` re-run after the drive confirmed green. The
flow, the verification gate, and the terminal drive all hold on the real stack. The drive is a credentialled, billable run
(documented in [`topologies/README.md`](../topologies/README.md)), not a committed
test.

*Finding (Iteration 4, the live drive) — two real bugs the structural tests
could not see, both about the model actually acting:*

1. **The brain got no tools (`fix(daemon)`).** The `llm` body advertises its
   `Emits` as the model's function menu, but the `BodyResolver` is handed only
   `(name, kind, config)` — never the subscriber's `Emits`. Through the
   descriptor/replay path the menu was therefore empty: Gemini received no
   functions and *narrated* prose ("I will read the file…", `input_tokens=116`,
   zero tool calls) instead of calling them. The apply test never caught it — it
   checks connectivity + catalog, not a real completion. Fix: the daemon mirrors
   the subscriber's `Emits` into the `llm` body descriptor (`mirrorLLMEmits`), so
   the rebuildable fact carries its own menu (G8). The subscriber's `Emits` stays
   the single source of truth.
2. **A non-tool turn was a dead end (`feat(llm)`).** After editing, the model
   replied in prose ("done") with no function call; nothing re-triggered it and
   the loop stalled at quiescence. The wrong fix is a prompt forbidding prose — it
   will not hold. The right fix is structural and *makes the no-function turn the
   completion claim itself*: a brain turn that calls no function emits an **event**
   — prose → `llm.message`, empty/silence → `llm.empty` (closing the G4 silent
   dead-end when a thinking model spends its whole budget reasoning). `llm.message`
   feeds the verifier→judge gate; `llm.empty` is a bounded re-prompt back to the
   brain. The `llm` body emits its answer kind, not a separate re-drive event, so a
   turn with both a tool call and prose advances once, never forking; and the scope
   budget on `llm.message`/`llm.empty` is the backstop for an endlessly chatty model.

These reinforce a §7 line: **completion is a verified state, never quiescence.**
A loop that "ends" because the model went quiet is a bug, not a terminal.

**Iteration 5 — External bash harness + first real instance.**
Pick one small-repo instance (requests/flask/pytest). Bash: pull/build the
instance image, `docker cp` the linux daemon+agent in, run the agent against
`/testbed`, `git diff` → `predictions.jsonl`, official harness. **First light.**

## 7. Settled decisions (do not relitigate)

- **New stack only.** The legacy stratum (`pkg/handler`, `pkg/bus`, `pkg/sdk`,
  `cmd/reflex`, `plugins/*`, `internal/runtime`, `examples/`, the retired
  `pkg/event`/`config`/`cost`/`cycle`/`graph`/`analyzer`/`projection`) has been
  **deleted** — it lives only in git history; reuse logic by porting from there.
- **All-in-container** for env; **Gemini on `iow-uagent`** first (Claude is an
  access question).
- **Events + subscribers**, no "tools/menu" concept.
- **Completion = verified state**, never an LLM claim or mere quiescence.
- **Benchmark glue is external bash**, never inside the framework.

## 8. Open questions specific to this target

- ~~**Control-plane depth for Iteration 1**~~: **resolved** — built the real
  self-hosted changeset-as-log-fold (`645a0bc`), not the thin slice. New
  follow-on (Iteration 2): an off-process client submits *ops* without Go
  `Decl`s, so it needs a name/kind → registered-body-factory binding (the
  plugin-`hello` seam of [`outdated/20`](./outdated/20-topology-management.md)).
- **Verifier target**: which test command the `verifier` runs for self-checking
  (repo default suite? a subset the brain names?), given the hidden tests are
  invisible.
- **Loop budget sizing**: tool-call budget per request for big repos (django) vs
  small (requests) — a scope `Budget` value, tuned per run, logged when it caps.
- **Model strength**: Gemini's low resolve rate caps the "resolved" tier; revisit
  if/when a strong-model GCP project is available.

## See also

- [`CONCEPT.md`](./CONCEPT.md) — the model, the built status, the vocabulary.
- [27 — state-defined agent](./27-state-defined-agent.md) — the agent shape.
- [28 — rebuild roadmap](./28-substrate-rebuild-roadmap.md) — engine-internal stages.
- [`outdated/20`](./outdated/20-topology-management.md) — the control plane this
  re-founds on the new engine.
