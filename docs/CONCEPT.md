# CONCEPT — the current reflex model, what is built, and the language

> **Status: CANONICAL / current. Read this first.** This is the single source
> of truth for the model as it now stands. It supersedes the prior
> consolidation ([24](./outdated/24-concept.md)) and the per-topic design docs
> (now in [`outdated/`](./outdated/)). The deep "why" still lives in
> [26 — bare substrate](./26-bare-substrate.md) and
> [27 — state-defined agent](./27-state-defined-agent.md); the live build plan
> in [28 — rebuild roadmap](./28-substrate-rebuild-roadmap.md). When an
> `outdated/` doc disagrees with this one, this one wins.
>
> **Vocabulary discipline (the thing that keeps drifting):** there are
> **events** and **subscribers (nodes)**. There is **no "tool" and no "tool
> menu"** as a concept — `tool.fs.read.call` is the *name of an event*, and the
> `fs` node is a *subscriber* to it. What a model "may do" is just its `Emits`
> allowlist. Completion is a **state reached by verification**, never an LLM
> claim and never mere quiescence.

---

## 1. What reflex is

An event-sourced agent kernel. The whole system is **three primitives** over a
single append-only log, animated by **two mechanisms** (append, dispatch).

**The invariant, never violated (G8 / uprightness): anything that cannot be
recomputed from the log is a bug.** Session state, scope status, the
subscription table, budgets, the event catalog, every view — all are *folds
over the log*, never stores. Only the engine writes the facts it owns
(`caused_by`, scope placement, closures); a reaction can never claim them.

## 2. The three primitives

```
Event       a record on the append-only log: { subject, trace, payload }
Reaction    a pure function (event, views) → emits; llm, tools, projectors — all Reactions
Projection  a declared fold over a causal horizon → a typed view
```

**The envelope is three parts** (the `terminal` flag was removed — it was the
last sender *opinion* in an envelope of facts):

| Part | Shape |
|---|---|
| `subject` | `{class}.{scope...}.{kind...}` — hierarchical dotted tokens, `*` one token, `>` tail |
| `trace` | `{ session_id, request_id?, span_id, caused_by[] }` — engine-stamped |
| `payload` | domain data, opaque to the engine |

- **Classes:** `sys.{kind}` (session-less machinery), `app.session.{id}.{kind}`
  (domain), `app.ingress.{surface}.{event}` (pre-resolution inbound).
- **`caused_by[]` is engine-stamped, never sender-chosen.** Membership in a
  scope is a fact of the log. `caused_by[0]` is the OTel parent, the rest are
  links (join nodes have N causes).
- **State paths live in the subject:** `state.updated.goal`,
  `state.updated.plan` — you subscribe to a *field's change*, not to a blob;
  the value is in the payload.
- **A `Reaction` returns `Emit{Kind, Payload}` only.** The dispatcher stamps the
  trace and places the scope — a reaction structurally cannot choose its
  event's scope, ancestry, or accounting.
- **Errors are events:** a reaction error becomes `{node}.failed` (non-terminal)
  into the cone; the drain continues, nothing unwinds (G3).

The two mechanisms: **append** (the sole write) and **dispatch** (fan the event
to the live table, stamping correlation, never branching on payload). The
engine is **payload-blind** everywhere except one place: runtime
payload-conformance against the catalog schema (§6).

## 3. Scope is a built-in projection, not a primitive

A **scope** is a root event plus the cone it dominates. Loops, barriers, phases,
sub-agent isolation, budgets, the request lifecycle — all one concept, none a
fourth primitive.

- **Membership is geometry, never addressing:** `E ∈ scope(R)` iff `R` is an
  ancestor of `E`, the path doesn't cross `scope.closed(R)` (sealing), and no
  nearer root lies on it (partition). Because `caused_by` is engine-stamped,
  events *land* in scopes by causality alone.
- **Quiescence is obligation counting** (Naiad-style, exact under the
  single-writer log): dispatch of `E` to N subscribers `→ +N` on every covering
  cone; a reaction completing `→ −1`; `obligations(R) == 0 → scope.{name}.closed`
  exactly once (G6). Dispatch is **depth-first** so a child increments its cone
  before the parent decrements (no false zero).
- **One state per scope instance** ([26 §2a](./26-bare-substrate.md)): the
  built-in fold of `state.updated.{path}` events in the cone into a kv
  (`path → payload`), born at the root, **frozen at close**. Per-instance ⇒
  isolation. Declared projections are *views over* states.
- **Writes are local; promotion is through closure only.** A node writes its own
  scope's state; the closing scope **carries its final state snapshot** in
  `scope.{name}.closed`, and a parent-scope consumer folds chosen fields up.
  Live cross-scope writes are impossible by geometry (a mid-cone emit is trapped
  in the cone). Promotion latency = scope granularity.
- **Closure ≠ completion.** Quiescence means the cone *froze*; terminality means
  it froze **on a terminal state value**. A cone that quiesces on a non-terminal
  state is a **stall** — a dead-end a consumer of the closure must handle. This
  is why "done" is a state predicate, not the engine going quiet (see §9).
- **Co-rooting is forbidden:** a span roots at most one scope. Two budgets over
  one cone = one scope with a multi-key `Budget` map.

## 4. Termination is a scope budget, not a node property

Termination does **not** come from node determinism. It comes from a **budget on
the covering scope** plus a **static cycle-coverage check**.

- Every non-trivial cycle (SCC) must be covered by a scope that declares a
  `Budget`; the validator rejects an uncovered cycle.
- The budget bites as a **per-cone cap** (the guarantee): dispatch stops
  admitting the bounded kind into an exhausted cone — the event is inert (on the
  log, in no cone), and a single graceful `scope.budget_exhausted` fact fires.
- **Connectivity × budget = termination:** connectivity says an exit exists;
  the budget says it is reached in bounded steps ([26 §3f](./26-bare-substrate.md)).

## 5. The LLM emits events, not tool-calls

An `llm` node is **not special** — it is an ordinary node with an `Emits`
allowlist whose `Body` happens to call a model.

- **There is no "tool" and no "menu" concept.** What the model may produce *is*
  the node's `Emits` allowlist. A "tool call" is an emission (`tool.X.call`) that
  happens to have a subscriber (`tool.X` node). Function-calling is transport
  encoding: the adapter decodes a function-call into `Emit{Kind:"tool.X.call"}`
  and a text completion into `Emit{Kind:"llm.message"}` — both allowlisted
  emissions.
- The model needs each emittable kind's **payload schema** — that is the event
  catalog (§6). A schema'd allowlist is all the model is handed.
- The new `llm` body emits **all** decoded function-calls (no "first call only"
  crutch); the engine handles parallel emits natively.

## 6. The event catalog — `kind → schema`, self-hosted

Events have **types**: the type of an event is its payload schema. The catalog is
a **type axis** orthogonal to wiring, not a fourth primitive.

- **Self-hosted** as a global-horizon projection over `event.registered{kind,
  schema}` facts; `event.registered` is the one primordial seed kind the engine
  knows natively. Static `EventKind` decls are the bootstrap form; runtime
  registration is just emitting the fact.
- **Three checks:** unknown-kind (`Emits` a kind absent from the catalog),
  dead-subscription (`On` pattern matching no catalog kind) — both static in
  `Validate`; and **payload-conformance** at runtime (a non-conforming emit
  becomes the body's own `.failed`). The catalog is **opt-in-until-adopted**:
  dormant while empty.
- The catalog schema **is** what the body hands the model as function
  parameters, so the allowed schema subset must be the intersection target
  providers accept (the LLM-tool-compatible subset).

## 7. View types — a projection carries a `Type`

A node reads views by name (`Reads`); a projection's `Type` selects how its
matched events become the view value ([26 §4b](./26-bare-substrate.md)).

- `Projection.Type` is an **open registry** (generalises the old final kv|log
  `Shape`). `kv` and `log` are built in; packages register more via
  `engine.RegisterType`. The engine does the payload-blind selection (backward
  `caused_by` walk + `On`-match → matched events); the registered **builder**
  does the type-specific shaping. A builder is still a pure fold of matched
  events → recomputable (G8).
- A node reads type-safely via `engine.ViewAs[T](views, name)`.
- **`llm.history`** is the first registered non-builtin type:
  `History{ System() string; Messages() []provider.Message }`. Its builder does
  the positional **system/message split** keyed to prompt-cache locality:
  boundary = first own-emit; pre-boundary task → first user message, the rest →
  frozen `System()`; post-boundary → append-only message tail, role by
  `Emits`-membership. Swapping prompt-assembly = swapping the view, not the body.

## 8. The control plane — topology is changesets on the log

Managing the agent (adding nodes, subscriptions, scopes, projections, event
types) is itself **events on the log** ([20](./outdated/20-topology-management.md)),
not a config-file edit + restart.

```
sys.topology.changeset.requested{ ops, principal }
   → engine validates fold(live table) + ops → resulting graph
   → facts (sys.node.registered, sys.subscribed, sys.scope.declared,
            sys.projection.registered, event.registered, …)
     + changeset.applied | changeset.rejected{ reasons }
```

- **The live table folds only facts, and only the engine writes facts** — the
  control plane's uprightness rule.
- **The resulting graph is validated, not each step;** a changeset applies
  atomically between dispatches. In-flight work drains naturally (obligation
  counting). Two severities: **reject** (structurally invalid) vs **lint**
  (`sys.lint.*`, subscribable).
- **One pipeline for every source:** boot YAML (a seeded changeset), admin CLI,
  hot reload, a plugin's `hello`. `reflex validate` is a **dry-run of the same
  validator** — boot/runtime/dry-run share one code path.
- **Declarations pin at the root; interventions target an instance** (extend a
  live request's budget/deadline, cancel it — audited, causally placed).
- Intended daemon surface: `reflex topo apply/show/diff/history`, `validate`,
  `scopes`, `emit`, with `--wait changeset.applied`. CLI, API, and YAML are all
  *clients* of the same changeset grammar, differing only by principal.

## 9. The state-defined agent

The agent is written as a **state model** compiled to a **flat list of
subscribers**; the engine validates connectivity
([27](./27-state-defined-agent.md)).

- **You write two things:** the task **state** (its fields and transitions) and a
  list of **subscribers** (`node + on + in + reads + emits`). The engine folds
  the `emit → subscribe` relation into a graph and reports gaps; at a gap it
  suggests an **LLM bridge**. Local edit, global check — you never hold the whole
  graph in your head.
- **LLM nodes are bridges**, not "a brain": each sits at a dead-end and emits the
  entry kinds of an otherwise-disconnected fragment. One universal `llm` body
  parameterised by config.
- **The task state** lives in the `request` scope: a `status` spine
  (`working/executing → done`, terminal) plus whatever the agent records (goal,
  plan, …).
- **Completion is a state reached by verification, not an LLM claim and not
  quiescence.** The flow is events + subscribers: an LLM node emits a *claim*
  ("I think it's done") → a **verifier** subscriber runs the checks (e.g. the
  tests) → `state.updated.status = done` is written **only if verified**, else
  back to `executing`. A **terminal node** on `state.updated.status` fires the
  terminal fact when the value is terminal — that is the terminal, and it is also
  the closure consumer that prevents a stalled close.
- **Parallelism is one node-set firing in N cones**, not N agents: one
  topology, N isolated `request` cones, isolation by `caused_by` geometry.

## 10. Language and conventions

- **Go.** Module `github.com/kgatilin/reflex`. Single binary surface; cobra for
  CLIs; zsh completions where a CLI ships.
- **Two strata — do not confuse them:**
  - **The new kernel (where all new work goes):** `engine/` (the three
    primitives + scopes + projections + catalog + view types + validator +
    dispatch), `nodes/llm` (the `llm` body), `nodes/tool` (`tool.Node`). It is an
    **in-process library** today: `New() → Apply(decls) → Append(ingress) →
    Drain(ctx) → Events()`. No daemon/bus/socket yet.
  - **The frozen legacy stratum (do NOT build on it):** `pkg/bus`, `pkg/handler`,
    `pkg/sdk`, `cmd/reflex`, `internal/runtime`, `plugins/fs`, `plugins/gotool`,
    `examples/agent.yaml`. This is the old shipped agent (daemon, socket plugins,
    eight-node-era handlers). The new kernel **replaces** it; reuse its *patterns*
    and *logic by porting*, never extend it in place.
  - **Shared:** `pkg/provider` — the neutral model interface and the real Vertex
    adapters (Gemini via `google.golang.org/genai`; Anthropic Claude via
    `anthropics/anthropic-sdk-go` + Vertex backend; OSS via Vertex
    OpenAI-compat). The new `llm` body calls it directly, so **real models work
    on the new stack** — `llm.New` resolves a real provider; only a stub is
    swapped in for tests.
- **Vocabulary:** events + subscribers; never "tool/menu" as a concept. State is
  central. "Done" is a verified state.
- **Discipline:** build/vet clean; test with `-race`; commit every
  self-contained unit immediately with a clear message; never push without an
  explicit ask. The engine must stay payload-blind except the one catalog
  conformance hook.

## 11. Implementation status (what is actually built)

All on branch `feat/connectivity-validation`, verified with `go test -race`
(whole repo green). Not pushed.

| Piece | Where | State |
|---|---|---|
| Connectivity validator (`Validate`/`Apply` dry-run) over a `[]Decl` graph; matrix checks via ArchMotif `pkg/graphval` | `engine/validate.go` | **done** |
| `Append` + `Drain` to quiescence (deterministic) | `engine/engine.go` | **done** |
| Scope instances, obligation counting, `scope.closed`, budget cap (graceful `budget_exhausted`) | `engine/scope.go` + `engine.go` | **done** |
| Projection evaluation (backward-walk views) + per-scope state + closure carries the state snapshot | `engine/projection.go` | **done** |
| Event catalog (`kind → schema`) self-hosted over `event.registered`; 3 checks | `engine/catalog.go` | **done** |
| View-type registry (`Type`, `RegisterType`, `Value`, `ViewAs`); validate unknown-type & dangling-reads | `engine/` | **done** (3a) |
| `llm.history` view type + `llm` body (real provider via `pkg/provider` → allowlisted emits + `llm.usage`); `tool.Node` | `nodes/llm`, `nodes/tool` | **done** (3b); a doc-27-style run reconciles on a stub provider |
| Real model adapters (Gemini / Anthropic / OSS on Vertex) | `pkg/provider` | **done** (shared); Claude blocked only by GCP publisher access, not code |

**Designed, not yet built on the new kernel** (the next work — see §12 and
[28](./28-substrate-rebuild-roadmap.md)):

- The **control plane as events** on the new engine: today `Apply(decls)` records
  into a slice, not a log fold. Doing it right (§8) = changeset facts +
  live-table-as-fold (the "management projections" family beside the catalog).
- A **daemon** around the new engine: send a message (emit ingress), run to
  terminal, read views, `--wait` — plus the CLI/API surface of §8 for real-time
  topology config.
- **In-process agent tools** for the new engine: `fs.{read,edit,write,search}`
  (port the legacy fs logic), and `go`/`py.test` as plugins.
- A **real-model agent run** (the coding agent of §9 with a verification flow),
  and an external benchmark harness (kept as throwaway scripts outside the
  framework).

## 12. Open questions (current)

- **Control plane on the new engine** (§8/§11): self-hosted topology-as-log-fold
  vs the pragmatic `Apply(decls)` slice first. ("Do it right" leans self-hosted.)
- **Completion via verification** (§9): the exact claim → verify → state flow and
  what the verifier checks beyond tests.
- **Tool schemas from the catalog** into the `llm` body (today advertised
  name-only); couple to the LLM-tool-compatible subset.
- **Context budget & view compaction** — the largest standing hole (carried from
  [24 Part II](./outdated/24-concept.md)): engine budgets count *events*, agents
  die of *tokens*; a compaction event as a horizon cut is unspecified.
- **Log payload weight** — `tool.fs.read.result` carries file content; decide a
  sha-keyed sidecar vs accept the log as a blob store.
- **Ingress-root detection vs dispatch match diverge** (found wiring the stage-3
  run): the validator marks an ingress root by the `app.ingress.*` pattern, but
  dispatch matches `On` against the kind tail — reconcile.
- **`provider.Message` is coarse** (`{Role, Text}`, no tool_use/result block
  pairing); proper tool-calling needs a richer message at the provider layer.

## See also

- [26 — bare substrate](./26-bare-substrate.md) — the "why" for §3–§7.
- [27 — state-defined agent](./27-state-defined-agent.md) — the "why" for §9.
- [28 — rebuild roadmap](./28-substrate-rebuild-roadmap.md) — the LIVE build plan.
- [`outdated/`](./outdated/) — the historical design journey (00–25), superseded
  in vocabulary and several mechanisms by this document.
