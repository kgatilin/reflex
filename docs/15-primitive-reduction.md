# 15 — Primitive reduction: two node types (DRAFT)

> **Status: DRAFT / proposed.** Converged in design discussion, not yet
> reflected in code and not yet folded into [00-reference](./00-reference.md)
> (whose node section still lists the eight-node vocabulary). This document
> supersedes that section once accepted. Builds on [11](./11-domain-model.md)
> (Reaction is the one concept), [12](./12-react-experiment.md) (the ReAct
> loop), [13](./13-event-taxonomy.md) (subjects/scope/trace),
> [14](./14-target-coding-agent.md) (the coding-agent target).

## Thesis

The builtin node vocabulary collapses to **two primitives**:

| `type` | Role | What it is |
|---|---|---|
| `llm` | **reason** | nondeterministic: folds the cone, calls the model, emits typed actions |
| `tool` | **act** | effectful: consumes `tool.{name}.call`, emits `.result` / `.failed` |

Everything else is either a **plugin** (a `tool` running out-of-process), `sys`
**machinery** (resolver, projections, dispatcher — runtime, not user
vocabulary), or it **disappears** into a direct subscription. This is
11-domain-model.md taken to its limit: Reaction is the only concept; a node
`type:` is just a reaction *body*, and there are exactly two non-pure bodies —
*reason* and *act*. The rest is topology.

## What folds away, and into what

| Retired node | Folds into |
|---|---|
| `decode` | `llm` — output parsing is config (the action allowlist); the merged node may still emit raw `llm.completed` as a terminal record for replay/cassette (12 F3) |
| `seed`, `pump`, `signal`, `forward`, `llm.turn` | **direct subscription** — the `llm` subscribes to its observation kinds; the glue and the turn-signal vanish (see below) |
| `router` | type-fork is already subscription (free); data/payload-fork is the `llm`'s job or a `tool` |
| `aggregate` | not a type — "subscribe to `scope.closed`" (see fan-out below) |
| `sink` | a fire-and-forget `tool` (a call with no result event: stdout, reply) |
| `tool_node` | just the `tool` contract running in-process; one tool concept, in-bus vs out-of-process is a deployment detail |
| `terminator`, `tool_call` | dropped — terminality is a flag any reaction sets; payload-routing retires |

`relay` (a pure rename `X → Y`) is **dropped**, to be revisited only if a real
wiring need appears; "rename an event at a subsystem seam" is usually a smell
(subscribe to the real kind instead).

## Direct subscription kills the loop scaffolding

In 12-react-experiment.md the ReAct loop needed `seed` (request → `llm.turn`)
and `pump` (tool result → `llm.turn`) nodes plus the `llm.turn` signal. All of
that is scaffolding. If the `llm` subscribes **directly** to its observations —

```
llm  consumes: [ request.received, scope.closed ]
```

— there is no seed, no pump, no `llm.turn`, no glue node. The `llm` folds the
cone and acts whenever a new observation lands. F4 (signals smuggling payload)
dissolves: there is no signal; the `llm` reads the cone, not the trigger.

## Fan-out synchronization is a projection, not a node

The open question this resolves: an `llm` turn emits N tool calls at once (N
events, all `caused_by` the completion); N plugins run in parallel; the `llm`
must take its next turn **once**, seeing all N results — not N times.

The turn **roots a sub-scope**. Its cone = {N calls + N results}. When all N
results are in, the cone **quiesces** — Dijkstra–Scholten termination
detection, which is the terminal-event invariant of 11-domain-model.md. The
progress projection emits **`scope.closed{root: the turn}`**. The `llm`
subscribes to `scope.closed` of its own turn, *not* to individual
`tool.*.result`, so it fires exactly once:

```
llm turn N  →  tool.a.call ┐
               tool.b.call ├─ sub-scope Tₙ
               tool.c.call ┘
                   ↓ (plugins, in parallel)
               tool.a.result ┐
               tool.b.result ├─ cone of Tₙ quiesces
               tool.c.result ┘
                   ↓
               scope.closed{Tₙ}      ← the barrier = a projection event
                   ↓
llm turn N+1  (folds the cone, sees all N)  →  sub-scope Tₙ₊₁  →  …
```

Three consequences:

1. **Synchronization is the progress projection** (already one of the two
   projection types in 11), not a new node primitive. "Aggregate" was never a
   type — it is "consume `scope.closed`".
2. **N=1 is the degenerate fan-out — no special path.** One tool per turn →
   a sub-scope of {1 call, 1 result} → `scope.closed` once. The `llm` always
   consumes `scope.closed` of its turn; sequential and parallel are one
   mechanism. The first turn roots on `request.received`.
3. **`scope.closed` is also the causal join, so no orphans and no deadlock.**
   Its `caused_by` is the cone's frontier (all N results/failures) — N causes →
   one event (OTel: a span with N links, per 13). A `tool.b.failed` (non-terminal
   error) does not stall the barrier: the cone still quiesces, the failure is in
   the closed cone's fold, and the `llm` sees it and decides. The failure is
   recorded, the next turn is caused by `scope.closed`, nothing is orphaned.

The loop is therefore a **chain of per-turn sub-scopes** — exactly "phases are
sub-scopes" from 11, with no self-invalidation: turn N+1 reacts to the closure
of Tₙ and opens Tₙ₊₁, never emitting back into the just-closed cone.

## The resulting model

```
llm     reason  consumes [ request.received, scope.closed ]
                emits    tool.{name}.call · assistant.message(T) · request.handled(T)
                         · llm.completed (raw, optional record) · llm.failed
tool    act     consumes tool.{name}.call
                emits    tool.{name}.result · tool.{name}.failed
                (plugin out-of-process, or in-bus for trivial pure fns)
```

`sys` machinery is unchanged from 00-reference (session-resolver, scope /
budget / orphan projections, audit, dispatcher) — it is what emits
`scope.closed`, `request.received`, `request.unhandled`, and the `sys.*`
control plane.

## Open / unresolved

- **How a sub-scope gets rooted.** Declared (`scopes:` block, 11) vs the
  runtime auto-rooting at any fan-out point (an event with >1 child). Auto-root
  is less config; declared is more explicit and statically analyzable.
- **Raw `llm.completed` for replay.** Keep the merged `llm` emitting it as a
  terminal record (cassette testing, 12 F3) or drop it and reconstruct from the
  decoded action. Leaning keep.
- **`relay` resurrection.** Whether any real seam needs a pure rename node, or
  whether direct subscription always suffices.
