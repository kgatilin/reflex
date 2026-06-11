# 19 — Projections: declaration, access, registration (DRAFT)

> **Status: DRAFT / proposed.** Closes the third-concept gap:
> [11-domain-model.md](./11-domain-model.md) names Projection
> ("fold(log) → view") but never specifies its interface — who declares
> one, how a reaction reads one, what the engine evaluates. Forced by the
> read-before-edit guard of
> [14-target-coding-agent.md](./14-target-coding-agent.md), whose reader
> is an out-of-process plugin. Builds on [13](./13-event-taxonomy.md)
> (subject grammar), [16](./16-engine-architecture.md) (scope geometry),
> [18](./18-pipeline-walkthrough.md) (state paths in the subject).
> Amendments to earlier docs close the document.

## The gap, and the residue

Doc 11's model is Event, Reaction, Projection. The first two have
contracts; the third has only a definition — and current code has the
*wrong* mechanism: `projection.Store` (doc 03) is a mutable KV that
handlers write (`Set`, `proj_set` over the wire) and read by key,
injected via `ProjectionAware`. A handler-writable store is pre-model
residue: in the converged model, writes belong on the log
(`state.updated.{path}`, doc 18) and reads belong to a declared fold.

The forcing case (doc 14): `tool.fs.edit.call` must fail unless **the
context in which the LLM emitted the edit contained a read of the file's
current version**. The guard reads payloads (paths, shas), so it cannot
be engine machinery (doc 16: domain-blind); its reader is the `fs`
plugin, out of process, so it cannot be "inspect the log yourself".
Whatever interface carries this case carries every projection.

## A projection is a declared fold over a causal horizon

```yaml
projections:
  - name: fs.seen
    on:    [tool.fs.read.result, tool.fs.edit.result, tool.fs.write.result]
    in:    session               # horizon — where the backward walk stops
    shape: kv                    # kv | log
    key:   payload.path
    value: payload.sha
```

The **reference evaluation is a walk**: from the read position (an
event), traverse `caused_by` ancestry backwards, collect events matching
`on:`, stop at the root of the nearest enclosing instance of the `in:`
scope. `shape` folds the collected set:

| shape | view | walk shortcut |
|---|---|---|
| `kv` | last value per `key` | first match per key on the backward walk |
| `log` | matching events, log order | full walk over the horizon |

What carries weight:

- **The declaration is the walk's instructions, not a data structure**:
  what kinds to look for (`on:`), what to extract (`key`/`value`), where
  to stop (`in:`). Incremental maintenance, per-scope snapshots, caches
  are all evaluation strategies for the same fold — G1
  (recompute-from-log) holds by construction because the walk is the
  definition.
- **`in:` is a horizon, not an instance registry.** With positions
  defined by causal ancestry there is no "one view instance per scope
  instance" to manage — the position determines the cone; `in:` only
  bounds how far back the fold reaches (`request`: this request only;
  `session`: the whole conversation). A reader outside any instance of
  the named scope gets an empty view.
- **Same grammar lattice as subscriptions** — {kind pattern} × {scope} —
  completing doc 16's symmetry line: handlers bind kind × wildcard
  scope, projections bind kind-pattern × horizon, scope-qualified
  subscriptions bind both.
- **`kv` over a partial order needs a tie-break.** Two causally
  incomparable branches may both carry a match (parallel reads of one
  path); "last" is undefined in causal order alone. Ties resolve by log
  position — the single-writer log is totally ordered (doc 17), so the
  result is deterministic and replay-stable.
- **Two shapes, deliberately final.** A fold beyond kv/log is not a
  projection — it is a reaction: subscribe, compute, emit
  `state.updated.{path}` (a terminal fact, path in the subject per
  doc 18), and the generic kv projection over `state.updated.>` serves
  the result. Materialization goes through the log — exactly what doc 18
  does with intent. Domain counters likewise; the engine's own counters
  (budgets) are structural and stay in the engine.

## Position: a read is anchored at the trigger

A reaction's views are evaluated **at its trigger event** — a fold over
the trigger's causal past. This yields the coincidence the
read-before-edit guard needs, as geometry rather than policy:

```
brain firing (trigger: scope.brain.closed)
  └─ tool.fs.edit.call          ← child of the firing
```

The call's causal past = the firing's causal past + the firing itself,
so `fs.seen` read at the *call's* position equals the view the *emitter*
consumed: **"the model had it in context" and "it is in the call's
causal past" are the same predicate.** The guard view and the context
view it protects must share declaration (kinds, horizon) — then the
coincidence is exact, with no second notion of "what the model saw".

Causal anchoring also gives isolation for free: parallel branches are
causally incomparable and do not see each other's reads, so there is no
view race to legislate; cross-branch interference is caught by the
world-check below.

## Access: `reads:` on the subscription, attached at dispatch

```yaml
- name: fs
  on:    tool.fs.edit.call
  reads: [fs.seen]
```

- The engine evaluates each declared view at the trigger's position and
  **attaches it to the delivery**: `deliver{event, views: {fs.seen: …}}`.
  Identical in-process and over the socket — a plugin never needs the
  log.
- The reaction contract becomes `React(event, views) → events`. **Raw
  log access retires.** The llm's transcript is itself a declared
  `log`-shaped projection it `reads:` — context assembly stops being
  magic inside the llm body and becomes the same interface.
- No query RPC from handlers: `proj_get`/`proj_set` retire. A reaction
  is a pure function of (event, views), so G5 replay reproduces every
  read exactly. Attach is the *contract*; a lazy walk with early exit is
  the natural evaluation, caching an optimization.
- Outside observers (CLI, audit, embedding API) may evaluate any view at
  any position — read-only, off the hot path.

## Registration: the control plane, same as handlers

`sys.projection.registered{name, on, in, shape}` — the exact lifecycle
of `sys.handler.registered` (doc 05). A `projections:` YAML block is
boot-time registration; a plugin registers its projections in the same
`hello` as its tools — the `fs` plugin brings `tool.fs.*` *and*
`fs.seen`, so read-before-edit ships with the tool that needs it, zero
engine changes. Precedent: the LLM tool menu was already "a projection
of the subscription table" (doc 11) — a kv fold over `sys.subscribed`;
even the engine's own derived structures fit this interface.

## The engine stays domain-blind — wording sharpened

Doc 16's anti-catalog says "no payload inspection". The precise rule is:
**the engine never *branches* on payloads.** Evaluating declared
selectors to materialize a view is blind copying — no engine decision
(routing, dispatch, closure, stamping) ever reads a domain view. The
dispatcher consults exactly one projection, progress, which reads
structure only. Domain views flow into reaction bodies and nowhere else;
enforcement built on them (the edit guard) is domain code in the
reaction, never a bus gate.

Cost discipline vs doc 17: "the frontier must be maintained
incrementally, never recomputed by traversal" governs the **progress**
projection — consulted at every dispatch. Domain views are read at
*reaction* granularity (once per delivery that declares them), where a
lazy backward walk with early exit is acceptable and the unbounded-DAG
concern is bounded by the horizon.

## Worked: read-before-edit, carried end to end

Declaration above. The `fs` plugin on `tool.fs.edit.call`:

```
seen := views["fs.seen"][call.path]
case seen missing        → tool.fs.edit.failed{ "no prior read" }
case seen != sha(disk)   → tool.fs.edit.failed{ "stale read — re-read the file" }
else                     → apply → tool.fs.edit.result{ path, sha }
```

- **Structural half (the projection):** a read/edit/write result for
  this path exists in the emitter's context. `on:` includes
  `edit.result` and `write.result` so a read → edit → edit chain does
  not force a pointless re-read — the model *did* see the latest version
  (it wrote it; the result event attests the sha).
- **Freshness half (the world):** the context's sha must equal the
  disk's, checked at execution time. A parallel branch or another
  session changing the file ⇒ mismatch ⇒ failed ⇒ the model must
  re-read. "The context did not contain the latest version", verbatim.
- **`base_sha` retires from the edit payload** (delta to doc 14): the
  log already carries the evidence; making the model thread the sha by
  hand duplicates the projection with a new failure mode (a hallucinated
  sha) and zero information gain. Every case doc 14 called it
  load-bearing for — parallel sessions, late writes — is covered by
  position + disk compare. Edit's signature shrinks to
  `{path, old, new, replace_all?}`.
- The audit fold of 12 W4 ("was every edit preceded by a read") is the
  same projection evaluated offline, at any position.

## Sessions are causal chains — the one new rule

For "seen in context" to hold across requests — and for doc 18's
`pending.confirm` fold, which already assumes it silently — a session's
requests must be causally connected. The resolver chains each new
request to the session's closed-request frontier:

```
request.received(n)  caused_by: [ ingress(n), scope.request(n-1).closed ]
```

(the first request chains to the session's binding event; if several
requests closed since, the new request carries all their closures —
`caused_by` is a list). Consequences:

- A session is **one connected cone**: "the conversation" is literally a
  causal thread, and `in: session` horizons walk it with no special
  case.
- Concurrent requests in one session are causally incomparable siblings —
  they honestly do not see each other mid-flight; their effects meet in
  the next request's chain.
- This is the only statement in this document that is new *model* rather
  than a derivation; everything else falls out of doc 16's geometry.

## What this buys beyond the case

1. **Data-flow edges in the static graph.** `consumes`/`emits` gave
   control flow; `reads:` gives data flow: node → projection → kinds →
   their emitters. Archmotif (07) and the optimiser (08) see "brain
   reads fs.seen, which fs feeds".
2. **The permissions hook (06).** "Who may write a path" is an
   emits-check on `state.updated.{path}`; "who may read" is a
   reads-check. Both static, both declaration-level — the doc 11 open
   question gets its second half.
3. **Wire simplification.** `deliver` carries positioned views, never
   the log; `proj_get`/`proj_set`/`--wait projection.has` retire in
   favour of view predicates over the same machinery.

## Amendments to earlier docs

| Doc | Amendment |
|---|---|
| 11 | "threshold crossings are announced" narrows to the *structural* projections (progress/budget). Domain projections never announce — a domain threshold is a reaction's job (subscribe, inspect, emit), the same logic that keeps closure predicates payload-free |
| 14 | `base_sha` retires; the read-before-edit guard is `fs.seen` + disk compare; "the guard is a fold over the cone" now has a carrier |
| 16 | anti-catalog wording: "no payload inspection" → "never branches on payloads"; "no state store" gains a clause: the engine *evaluates* declared folds but holds no view a position cannot recompute |
| 17 | "never recomputed by traversal" scoped to the progress projection; domain views may walk lazily at reaction granularity |
| 03 | `projection.Store`'s mutable API, `ProjectionAware`, `proj_set`/`proj_get`: pre-model residue, retired by this interface |

## Deltas in current code

1. `React(ctx, ev, log)` → `React(ctx, ev, views)`; the SDK `deliver`
   frame gains `views`.
2. A `projections:` config block + `sys.projection.registered`
   control-plane kind; plugin `hello` extended to carry projection
   declarations.
3. The walk evaluator (backward `caused_by` traversal with horizon and
   early exit) and the kv/log folds with declarative selectors.
4. Session chaining in the resolver (`caused_by` gains the
   closed-request frontier).
5. Retirements: `projection.Store` writes, `ProjectionAware`,
   `proj_set`/`proj_get`, `--wait projection.has` (replaced by a view
   predicate).

## Open / unresolved

- **Evaluation crossover.** At what session length does the lazy walk
  lose to incremental snapshots cached at scope roots? Implementation
  only; the contract is unaffected.
- **Wire weight of `log` views.** The transcript attached to every llm
  delivery is large; delta-encoding against the previous delivery is the
  obvious fix, semantics unchanged.
- **Tie-break visibility.** kv ties between incomparable branches
  resolve by log order — should the view flag that a tie occurred?
  Lean: silent; the audit fold can always detect it.
- **Empty-horizon reads.** A reader outside any instance of its view's
  `in:` scope gets an empty view — or a wiring-time error? Lean:
  wiring-time lint, runtime empty view.
