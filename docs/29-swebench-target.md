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

**Iteration 2 — Daemon around the new engine + send-message/wait.**
A long-lived process hosting the engine: ingress (`emit`), drive to terminal
(the `request.terminal` fact / a terminal-state predicate), read views, `--wait`.
The management surface of §1's control plane as CLI (`reflex topo
apply/show/diff`, `emit`, `validate`, `scopes`) **and** an API, so the daemon is
configurable live. (Socket transport for out-of-process plugins can follow; tools
may start in-process.)

**Iteration 3 — The hands: file/test subscriber nodes + catalog schemas.**
`fs.{read,edit,write,search}` (port the legacy fs logic, in-process,
root-confined), `py.test` (shell out to pytest), `go.*` if useful — each a
subscriber node. **Register their event kinds + parameter schemas in the catalog**
so the `llm` body advertises real function schemas (closes the `CONCEPT.md` §12
"tool schemas from catalog" gap). Schemas must be the LLM-tool-compatible subset.

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

- **New stack only.** Legacy (`pkg/handler`, `pkg/bus`, `pkg/sdk`, `cmd/reflex`,
  `plugins/*`) is **frozen** — reuse logic by porting, never extend it.
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
