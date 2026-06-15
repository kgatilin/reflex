# 28 — Substrate rebuild roadmap: from connectivity validation to a running reconciler (LIVE)

> **Status: LIVE / operational.** The staged plan that turns the converged
> substrate (docs [24](./outdated/24-concept.md)/[26](./26-bare-substrate.md)) into a
> running state-defined agent (doc [27](./27-state-defined-agent.md)). Doc 27
> is the design capture; **this is the tracker the rebuild runs from** — what
> shipped and the next stages, updated as each lands. Mirrors the
> [22](./outdated/22-bootstrap-self-hosting.md)(plan)/[23](./outdated/23-bootstrap-roadmap.md)(live)
> split, one substrate generation later.

---

## What shipped

| Piece | Where | State |
|---|---|---|
| Engine contracts: `Event`/`Trace`/`Emit`, `Reaction`/`Views`/`KV`, `Decl` (`Node`/`Scope`/`Projection`/`EventKind`), `tool.Node`, `llm` | `engine/`, `nodes/` | **done** (llm body shipped, stage 3) |
| **Step 1 — connectivity validation** (doc 27 §7) | `engine/` + ArchMotif `pkg/graphval` | **done, verified** |
| **Stage 2a — `Append` + `Drain` to quiescence** (deterministic) | `engine/` | **done, verified** |
| **Stage 2b — scope instances, obligation counting, `scope.closed`, budget cap** (docs 24 §5 / 26 §3d/§3f) | `engine/scope.go` + `engine/engine.go` + `validate.go` | **done, verified** (`306fc6e`); co-rooting reject (`47dcf3d`) |
| **Stage 2c — projection evaluation (backward-walk views) + per-scope state + closure carries snapshot** (docs 26 §2a / 24 §6) | `engine/projection.go` + `engine.go` + `scope.go` | **done, verified** (`e8f5c55`); also fixed a latent multi-ingress drain defect (frontier → per-index dispatched) |
| **Stage 2d — event catalog (`kind → schema`) self-hosted over `event.registered`; unknown-kind / dead-subscription / payload-conformance checks** (doc 26 §4a) | `engine/catalog.go` + `topology.go` + `validate.go` + `engine.go` | **done, verified** (`2fdee6e`); catalog validation later made **always-on** (opt-in dormancy removed) with the engine self-registering its own kinds (seed + per-scope `scope.*.closed`/`.budget_exhausted`) |
| **Stage 3a — open the projection view type** (`Shape`→`Type` registry; `RegisterType`/`Views.Value`/`ViewAs[T]`; validate unknown-type & dangling-reads) (doc 26 §4b) | `engine/topology.go` + `projection.go` + `reaction.go` + `subject.go` + `validate.go` | **done, verified** (`fd794d3`); `KindOf`/`MatchKind` exported for builders |
| **Stage 3b — `llm.history` view type (positional system/message split) + `llm` body (provider call → allowlisted emits + `llm.usage`)** (doc 26 §4/§4b) | `nodes/llm/llm.go` (+ history/run tests) | **done, verified** (`8b04898`); a doc-27-style run reconciles `new→…→task.answered`, scope closed once, usage per seat, on a stub provider (`-race`) |
| **Control plane — topology is a fold of changeset facts** (doc 29 Iteration 1 / doc 20 / CONCEPT §8): `Apply` = the in-process changeset client (`requested → facts + applied \| rejected`); live table is `foldTopology(log, bodies)`, bodies resolved by name from a process registry; `Topology()` read + `install()` test seam | `engine/changeset.go` + `engine.go` | **done, verified** (`645a0bc`); rejected changesets write no object facts; live table round-trips as a fold of the log (G8); pipeline-applied topology drives a real run to terminal + closes once (`-race`) |
| **2a — serializable body descriptors + factory registry** (doc 29 Iteration 2): `Node.BodyKind`+`BodyConfig` ride on `sys.subscriber.registered`; `engine.BodyResolver` + `WithBodyResolver`; `engine.Load(log)` rebuilds bodies from the log; `nodes.Register`/`Resolver`; `llm.Factory`/`Declare` | `engine/resolver.go` + `nodes/registry.go` + `nodes/llm` | **done, verified** (`79fb036`); a descriptor topology recovers from the log alone and runs; unknown-kind/no-resolver rejected (`-race`) |
| **2b — daemon + CLI/API** (doc 29 Iteration 2): declarative topology document (`pkg/topology`, YAML/JSON → decls); engine host (`pkg/daemon`) with a unix-socket HTTP API (apply/validate/emit/topology/events); `reflexd` CLI (serve/apply/emit --wait/topology/validate) | `pkg/topology` + `pkg/daemon` + `cmd/reflexd` | **done, verified** (`5baf443`, `52b0201`); server↔client round-trip reconciles to terminal, rejection carries the gap report (`-race`); in-memory log (disk persistence deferred) |

**Step 1 detail.** `engine.Validate(decls...) → Report{Connected, DeadEnds,
UnreachableNodes, Fragments, UnboundedCycles, Suggestions}`; `Apply` routes
through it (returns `*ValidationError` on a gap, never touches the log).
Added scope-defaulting (empty `In` → `global`) and the domain matcher
`subjectMatch` (hierarchical
dotted tokens, `*` one token, `>` tail — **no technology name in the
domain**). The topology graph is built from `[]Decl` (edge `X→Y` iff a kind
in `X.Emits` is `subjectMatch`-ed by a pattern in `Y.On`) and all five
checks run as **matrix algebra over a graph in ArchMotif's `pkg/graphval`**
(closure / reachable-from-roots / SCC / dead-items) — reflex builds the
graph, never sees the matrix. The unbounded-cycle check is **scope-budget
coverage** (every cycle within a budgeted scope); a `Node.Kind` tag was added
then removed — determinism does not give termination (doc 26 §3).

**Branches (local only, not pushed/merged):** reflex
`feat/connectivity-validation` (`057489b`); ArchMotif
`feat/public-matrix-validators` (`e94c8d9`, public `pkg/graphval`,
graph-in contract, matrix internal). reflex depends on it via
`replace github.com/kgatilin/archmotif => ../tools/archmotif`.

## What this roadmap rests on (decisions, with pointers)

- **Substrate = events + states (projections) + reactions**; scope is a
  built-in projection, not a fourth kind; **termination is a scope budget**
  (a static cycle-coverage check, no node attribute), not a dispatch gate
  ([26](./26-bare-substrate.md)).
- **The LLM has no tools** — it emits allowlisted events; a tool-call is an
  emission with a tool-node consumer; the "menu" is just `Emits`
  ([26 §4](./26-bare-substrate.md)).
- **One state per scope** ([26 §2a](./26-bare-substrate.md)): writes are local
  to the writer's scope; promotion to a parent/`global` is **only through
  closure** (the closing scope carries its final state, a parent consumer
  folds it up); promotion latency = scope granularity. No live cross-scope
  write.
- **Event catalog on a type axis** ([26 §4a](./26-bare-substrate.md)):
  `kind → schema`, self-hosted as a projection over `event.registered`
  (one primordial seed kind). The catalog is what the `llm` body advertises;
  it is Event + Projection, no new primitive.
- **Validation is the engine's job**, expressed as matrix algebra over a
  graph; the contract to ArchMotif is a graph (doc 27 §5).
- **The domain is technology-agnostic** — `subjectMatch` is ours; the
  "NATS grammar" wording in [24 §2](./outdated/24-concept.md) is a leak to clean up
  when docs are next touched.

## Stages

### Stage 2a — `Append` + `Drain` to quiescence (deterministic only)

The smallest runnable slice — make a reaction chain actually flow.

- **`Append`**: stamp span id, resolve session, derive request id, put the
  event on the log (the sole write).
- **`Drain`**: dispatch each undispatched event to matching subscribers
  (`subjectMatch` over `On` + scope match over `In`), run the reactions
  (`Views` empty for now), stamp `caused_by` on their emits, append, repeat
  to fixpoint (global quiescence).
- **No scopes, no views, no llm.**
- **Acceptance**: an acyclic deterministic topology
  (`resolver → deterministic → tool → terminal`) appended once, drained,
  with the expected events asserted on the log.
- **Proves**: routing by subject + scope, and the
  reaction → emit → append cycle end-to-end.

### Stage 2b — scopes + obligation counting + closure

The doc-24 §5 / doc-26 runtime.

- Scope rooting (declared + node-rooted), obligation counting per cone,
  `scope.X.closed` emitted exactly once at quiescence (G6), and **scope
  budgets that bound cycles**. The threshold decision is **resolved**
  ([26 §3d](./26-bare-substrate.md)): a per-scope **cap** is the guarantee
  (dispatch stops admitting the bounded kind into an exhausted cone), with a
  terminal `budget_exhausted` fact for graceful shutdown. `closed` thus has
  two predicates into its final value — `obligations == 0` or
  `counted_kind == budget`.
- The validator's **stalled-closure check** ([26 §3f](./26-bare-substrate.md),
  [27 §5](./27-state-defined-agent.md)): `scope.X.closed` is an emitted kind;
  a close that can land non-terminal needs a continuation (LLM bridge) or a
  provably terminal-only close. Connectivity (exit exists) × budget (exit
  reached in bounded steps) = the reconciler's termination guarantee.
- **Acceptance**: a loop topology (`gather ↔ fs`) covered by a budgeted scope
  runs to a `scope.X.closed` and terminates; a stalled close re-drives via a
  bridge into a new child cone and still converges under its covering budget;
  a join (N tool calls) closes once when all results are in (N=1 is the
  degenerate case).
- **Proves**: loops, joins, scope budgets, stall→bridge re-drive — the heart
  of the model.

### Stage 2c — projections + the per-scope state ([26 §2a](./26-bare-substrate.md))

- **One state per scope instance** (the built-in fold): each scope's
  `state.updated.{path}` events fold into one kv (`path → payload`), born at
  root, frozen at close. `task_state` *is* the `request` scope's state, not a
  free-standing projection.
- **Declared projections are views over states**: kv/log views via a backward
  walk over `caused_by` to the horizon (`request` / `session` / `global`),
  attached as `Views` at dispatch ([24 §6](./outdated/24-concept.md)); a view may join
  the node's own scope state with an ancestor/`global` state.
- **Closure carries the state snapshot**: extend the 2b `scope.X.closed`
  payload to include the cone's final state, so a parent-scope consumer can
  fold chosen fields up — the only request→global promotion path (writes are
  local; no live `sys.` up-write).
- **Acceptance**: a reaction reads a kv/log view reproducing the expected
  fold; read-at-trigger isolation holds across parallel `request` cones; a
  `global` consumer of `scope.request.closed` promotes a field into the
  global state.
- **Proves**: the reconciler's state becomes computable from the log, and
  state flows up through closures, never sideways.

### Stage 2d — event catalog (`kind → schema`, self-hosted)

- **`event.registered{kind, schema}`** is the one primordial seed kind (schema
  built-in); the **catalog** is the global-horizon projection folding these
  into `kind → schema` ([26 §4a](./26-bare-substrate.md)). Dynamic registration
  = emit a registration fact; static bootstrap seeds them before ingress.
- **Three validator checks** ([27 §5](./27-state-defined-agent.md)):
  unknown-kind (an `Emits` not in the catalog), dead-subscription (an `On`
  matching no catalog kind), and runtime payload-conformance (an emit whose
  payload violates its kind's schema → the body's `.failed`).
- **Acceptance**: registering a kind makes it emittable/advertisable; an
  unregistered `Emits` and a never-matching `On` both fail validation; a
  malformed payload becomes `.failed`, not a crash.
- **Proves**: the type axis is self-hosted (Event + Projection, one seed),
  and the `llm` body can advertise `Emits` + schema with no "tool" concept.

### Stage 3 — `llm` body + run the reconciler — **DONE** (`fd794d3`, `8b04898`)

- **Prerequisite — the event catalog** ([26 §4a](./26-bare-substrate.md), built
  in Stage 2d): an `llm` body advertises to the model *its `Emits` allowlist,
  each kind with its catalog schema* — there is no separate "tool menu", the
  allowlist **is** the menu, and the schema comes from the catalog. An LLM
  node is not special — it is a node with `Emits` whose body calls a model.
- **Prerequisite — the view type** ([26 §4b](./26-bare-substrate.md)): a
  projection carries a `Type` (rename `Shape` → `Type`, open registry). `kv`/`log`
  are built-in; the `llm` package registers `llm.history` →
  `History{ System() string; Messages() []provider.Message }`. Engine adds
  `RegisterType(name, builder)`, `Views.Value(name) any`, generic
  `ViewAs[T](views, name)`; `KV`/`Log` stay as sugar. `Reads` is unchanged (the
  projection name *is* the implementation). Validation: `Reads` names resolve;
  `Type` is registered.
- The default `llm.history` builder does the **positional system/message split**
  ([26 §4b](./26-bare-substrate.md)): boundary = first own-emit (kind ∈ `Emits`);
  pre-boundary minus task → frozen `System()`; task → first user message;
  post-boundary → append-only log-order tail, role by `Emits`-membership.
- `llm.New`: read its declared `llm.history` view (`System()`/`Messages()`),
  build `provider.Request` (tools = `Emits`∖answer-kind × catalog schema), call
  the provider (stage-0 `pkg/provider`, reused), decode the completion **and**
  function-calls into allowlisted `Emit`s ([26 §4](./26-bare-substrate.md):
  function-calling is transport encoding, not a "tool" mechanism — every decode
  is an allowlisted emission); answer-kind fixed = `llm.message`; always emit
  `llm.usage`.
- Wire the doc-27 §3/§4 topology; run a real task through
  `new → gathering → planning → executing → done`.
- **Acceptance**: the example topology, given a task, reconciles to
  `done | needs_clarification` with cost logged (`llm.usage` →
  `reflex costs`).
- **Proves**: the whole doc-27 model runs.

## Sequencing rationale

- Scopes can't be tested without dispatch → **2a first**.
- Views are needed by the `llm` body, not by deterministic/tool nodes →
  **2c before 3**.
- The `llm` body is **last** — validate the entire runtime on deterministic
  nodes before adding the model's nondeterminism.

## Open / deferred (carried)

- Cycle budget-coverage is approximated as "every SCC node is `in:` a
  budgeted scope"; the precise rule (budget bounds a kind on the cycle's
  edges) is a refinement.
- **2b closure ordering** is correct under depth-first dispatch (every
  ancestor cone holds an obligation until its own root leaves, so nested
  cones never quiesce on the same event — narrowest closes first).
- **Co-rooting is forbidden** (validator check `coRootedScopes`): a span roots
  at most one scope. Two scopes rooted on one event share an identical cone
  and add nothing over a single scope with several `Budget` entries — so
  "two budgets over one cone" is one scope with a multi-key `Budget` map, and
  the validator rejects two scopes whose root triggers overlap
  ([26 §3d](./26-bare-substrate.md)). Trigger-overlap is approximated
  token-wise (a ">" tail is treated as conservatively overlapping; erring
  toward rejection is correct for a hard constraint).
- 2b leaves `request_id` keyed on the literal scope name `request`
  (`narrowestRequest`); a broader "request-class" notion would need a
  convention.
- Static allowlist lint (`emit ⊆ Emits`) — runtime-only today, a stub.
- **Catalog schema = LLM-tool-compatible JSON Schema.** A catalog kind's schema
  is exactly what the body hands the model as function parameters, so the
  allowed subset must be the *intersection* of what target providers accept
  (e.g. `oneOf`/`allOf`/`$ref` often unavailable). The 2d in-package validator
  (top-level `type:object` + `required` + one-level `properties[].type`) is a
  start; tighten it to the provider-tool subset when wiring stage 3.
- ~~**`app.ingress.*` is the perimeter.**~~ **RESOLVED**: the `app.ingress`
  subject class was removed. There is no perimeter namespace — an external event
  is a plain registered domain event, and a root is structural (a subscriber
  whose `On` matches a kind no subscriber produces — `isRoot`). The external
  entry kind is registered in the catalog like any other.
- **Stage-0 `provider.Message` is coarse** (`{Role, Text}`, no tool_use/
  tool_result block pairing): tool results flatten into user text. Fine for the
  stub run; proper tool-calling needs a richer `provider.Message` (provider
  layer, not engine). `provider` is the egress adapter the `llm` body calls — it
  is below the three primitives, not one of them. STILL OPEN.
- ~~**Ingress-root detection vs dispatch match diverge**~~ **RESOLVED**: with the
  `app.ingress` class gone, `splitSubject` no longer strips a prefix, so the
  validator and the dispatcher match the same string. Root detection is
  structural (`isRoot`), not pattern-based, so the
  `["app.ingress.*", "cli.task"]` double-pattern workaround is deleted.
- **Tool schemas not yet wired into the llm body**: tools are advertised by
  name only (`provider.ToolSchema{Name}`), schema empty — the catalog→tool-param
  schema wiring (a catalog kv-view the body reads) is deferred; the stub ignores
  schemas. Couple to the LLM-tool-compatible subset (above) when adding a real
  provider.
- **`historyParams.Answer` is carried but unused by the builder** (role is by
  Emits-membership); kept for a future richer role model (e.g. source-based
  perspective for multi-agent cones).
- `pkg/graphval` vs `graph` naming (chose `graphval` — ArchMotif already
  has `internal/graph`); reflex-side import could alias to `graph` if
  preferred.
- [24 §2](./outdated/24-concept.md) "NATS grammar" wording cleanup (domain-agnostic).
- Merge/push policy: the two feature branches stay local until the operator
  decides to merge.

## See also

- [27-state-defined-agent.md](./27-state-defined-agent.md) — the design and
  the subscriber-list compilation this roadmap builds out.
- [26-bare-substrate.md](./26-bare-substrate.md) — scope as projection,
  termination as a scope budget (cap + graceful fact, §3d) with the
  connectivity×budget closure guarantee (§3f), the LLM emits events.
- [24-concept.md](./outdated/24-concept.md) — the settled model; §5 (scopes/
  quiescence), §6 (projections).
- [23-bootstrap-roadmap.md](./outdated/23-bootstrap-roadmap.md) — the prior-generation
  live roadmap this one supersedes in track.
