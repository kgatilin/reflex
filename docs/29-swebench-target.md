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
| `resolver` | `app.ingress.*` | `global` | `request.received{task}` (roots `request`) |
| `brain` (llm, **loops**) | `request.received`, `tool.fs.*.result/failed`, `tool.py.test.result/failed`, `verify.failed` | `request` | `tool.fs.read/edit/write/search.call`, `tool.py.test.call`, `claim.complete`, `llm.usage` |
| `fs` (subscriber) | `tool.fs.>.call` | `request` | `tool.fs.*.result/failed` |
| `pytest` (subscriber) | `tool.py.test.call` | `request` | `tool.py.test.result/failed` |
| `verifier` | `claim.complete` | `request` | runs the agreed test command; green → `state.updated.status{done}`, red → `verify.failed` |
| `terminal` | `state.updated.status` | `request` | when terminal → `request.terminal` |
| `costs` (sink) | `llm.usage` | `request` | — |

- **The loop is event closure**, bounded by a `Budget` on the `request` scope
  (the tool-call kinds) — the termination backstop (`CONCEPT.md` §4). `brain`
  reads its `llm.history` view (`CONCEPT.md` §7); the issue text is the first
  user message, file/test results are the appended tail.
- **Completion is verified, not claimed.** `brain` emits `claim.complete` when it
  *thinks* it is done; `verifier` re-runs the tests deterministically and writes
  `status=done` **only if green**, else `verify.failed` back into the loop. "Done"
  is a state the verifier sets, surfaced by `terminal` — exactly closure ≠
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
`sys.node.registered` … facts** (`foldTopology(log, bodies)`), not the old
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
`apply`/`validate` alongside the operator document, so the **resulting graph is
validated as a whole** — a launched-but-unwired plugin is correctly a gap
(unreachable / dead-end), and connectivity is a property of the assembled graph,
never of a lone handler. The schemas come from the plugin and land on the log as
`sys.event.registered` facts; `reflexd` hardcodes nothing.

The subscriber's scope is **`global`** on purpose: a plugin is scope-agnostic —
it handles its kinds wherever they occur, and the engine places its emits in the
trigger's cone by causality (a reaction never chooses its emit's scope, doc 24
§5). `engine.Load` rebuilds the subscriber node from `sys.node.registered` and
re-spawns the process from its descriptor without re-launching (G8).

Properties this preserves: the engine's **emit-allowlist still binds the plugin**
(out-of-process is not out-of-bounds); the body descriptor (`kind` + spawn
command) rides on `sys.node.registered`, so the wiring is **recoverable from the
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

*Finding (3a):* adopting the catalog — a plugin contributing even one kind —
flips on **full catalog enforcement** (`validate.go`: a non-empty catalog gates
`Connected`), so the whole topology must then declare every kind, including
engine-internal `scope.*.closed` and ingress kinds. This is the right end-state
for the agent (we want a full catalog so the `llm` body can advertise function
schemas), but a follow-up should **auto-register scope-closure + ingress kinds**
to cut operator boilerplate.

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

**Iteration 4 — The coding-agent topology + verification flow, run locally.**
Express §4 as a changeset/YAML; wire the real model (Gemini, `iow-uagent`,
`location global`). Run a *synthetic* coding task on a local checkout end-to-end
— prove the loop, the verification flow, and the terminal-state drive on the real
stack, no benchmark yet.

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
