# 08 — Optimization as rewrite

Phase 6. Compression passes are graph rewrites expressed as control-plane
event sequences. The same objective scalar the Phase 3 analyzer
minimises drives the optimisation loop. Human feedback enters the
system as ordinary rules (handlers); scope guards prevent the optimiser
from rewriting human-added rules without an explicit gate.

## The principle

A reflex topology is a graph: nodes are handlers, edges are
event-type bindings. Optimisation is *graph rewriting under a
behavioural-equivalence constraint*:

```
minimise |rules|
subject to:   behavioural equivalence on the trace corpus
              + scope guards (Phase 4c)
```

Where `|rules|` is the size of the current ruleset (handler count +
subscription count) and behavioural equivalence is "for every request
in the trace corpus, the observable outputs (terminal events,
projection state) are unchanged".

The optimiser is a handler (the archmotif subscriber of Phase 7) that
proposes rewrites via `CompressionProposed{patches: [...]}`. The
patches are sequences of `HandlerRegistered` / `Subscribed` /
`Unsubscribed` / `HandlerDeregistered` events that, when published,
mutate the live table.

## Compression passes

### Subsumption

When `A.consumes ⊂ B.consumes` and `emit overlap is empty`, A's role
is redundant: replace A's subscriptions with B's:

```
detection:
  A.subscriptions = {T1, T2}
  B.subscriptions = {T1, T2, T3}
  A.emits ∩ B.emits is empty  (no behavioural overlap)

patch:
  HandlerDeregistered{A}
  (no new subscriptions needed — B already covers A's domain)
```

### Dead-edge prune

A subscription that has never fired in the trace corpus is dead.
Remove it:

```
detection:
  subscription_count[X, T] / total_subscriptions_observed[T] = 0

patch:
  Unsubscribed{handler: X, event_type: T}
```

This is the cheapest pass and the safest — removing a subscription that
the corpus shows is never used cannot change behaviour on traces that
match the corpus.

### Pass-through compression

When the chain is `A → B → C` and B's only role is to forward (B's
emissions are deterministically derivable from its inputs without any
state), collapse:

```
detection:
  B.consumes ∩ B.emits has a 1:1 mapping (no aggregation, no filtering)
  B is stateless

patch:
  HandlerRegistered{A → C bypass}  (or amend A to emit C's type directly)
  Unsubscribed{handler: B, event_type: A_emit_type}
  HandlerDeregistered{B}
```

Behavioural equivalence holds when B's contribution to the trace is
purely a label change.

### Cluster collapse

A strongly-coupled subgraph (several handlers exchanging a high-volume
event family) collapses into a single composite handler. This is where
archmotif's `LayerCollapseOp` (Phase 4d `pkg/metrics` shim) carries
the matrix-level analysis:

```
detection:
  LayerCollapseOp identifies SCC {X, Y, Z} with internal traffic ≫
  external traffic (high coupling, low fan-out outside the cluster).

patch:
  HandlerRegistered{name: XYZ_composite, type: composite,
                    consumes: union(external inputs), emits: union(external outputs)}
  Subscribed{XYZ_composite → <every external input type>}
  Unsubscribed{X → ...}, Unsubscribed{Y → ...}, Unsubscribed{Z → ...}
  HandlerDeregistered{X}, HandlerDeregistered{Y}, HandlerDeregistered{Z}
```

The composite handler internally re-implements the union of `X`, `Y`,
`Z`'s behaviour on the shared event family without round-tripping
through the bus. Internal traffic shifts from "events on the bus" to
"function calls inside the composite" — depth in the causal DAG drops
proportionally.

## Cost function

The optimiser's cost function is the Phase 3 objective scalar
(`pkg/analyzer/objective.go`):

```
Cost = max(per-request causal_depth)
     + 1000 * orphan_count
     + 1000 * mis-terminated_request_count
     + 1000 * cycling_node_count
```

A patch is accepted only when its predicted post-rewrite `Cost` is
lower than the current `Cost`. Replays over the trace corpus check the
prediction: simulate the rewrite against the corpus, recompute, compare.

The 1000× penalty ensures that no rewrite can be accepted if it
introduces an orphan, breaks termination correctness, or creates an
uncapped cycle. The optimiser is structurally prevented from
"optimising" the system into an invalid state.

### Behavioural equivalence

The trace corpus is a sample of past `reflex run` outputs. For each
request in the corpus, the optimiser computes:

```
before: (terminal_events, projection_state) under current topology
after:  (terminal_events, projection_state) under proposed topology
```

The rewrite is *equivalent on the corpus* when `before == after` for
every request. Differences (different terminal types, different
projection keys) reject the rewrite.

Equivalence on the corpus is not equivalence in general — a request
outside the corpus distribution might behave differently. Phase 6
accepts this as the working approximation; a future phase can add
formal-equivalence checks for restricted handler classes.

## Feedback as rule

Human feedback enters the system as a normal handler:

```yaml
- name: human_clarification_rule
  type: feedback_rule
  on: TriageDecided
  scope:
    mutate: [feedback.clarification]
    read:   [triage.*]
  config:
    rule: "if classification == 'STUCK' and not labelled 'agent-ready', do not auto-escalate"
```

The rule is itself a handler subscribed to the relevant event. It is
created via the same `HandlerRegistered + Subscribed` events as any
other handler. The optimiser's pattern-matching treats human-added
rules the same as machine-added ones — they participate in subsumption,
dead-edge prune, pass-through compression alongside the autonomous
handlers.

### Scope guards on feedback

Phase 4c reserves `feedback.*` as a human-owned namespace. The
optimiser's `forbidden: [feedback.*]` prevents direct mutation of
feedback rules. To rewrite a feedback rule, the optimiser must:

1. Emit `SuggestedRewrite{target: feedback.clarification, patches:
   [...], reason: "subsumed by feedback.auto_clarify"}`.
2. The human-gate handler (subscribed to `SuggestedRewrite` on
   `feedback.*`) presents the patch to a human.
3. On `HumanGateApproved{suggested_rewrite_id}`, the patch's
   `HandlerRegistered` / `Unsubscribed` events are emitted — under the
   gate's scope (which DOES include `feedback.*` mutation).
4. On `HumanGateRejected`, the suggestion is logged and dropped.

Same mechanism as the auto-acceptance path, with a human in the loop.
The audit log records every step.

This is the structural difference between human-added and
machine-added rules: machine-added rules can be transparently rewritten
under their own scope; human-added rules require an explicit gate.
Without scope guards, the optimiser would converge by deleting
human-added rules whose objective contribution it couldn't measure.

## Why this is the same engine

Phase 3 ships a function `Compute(trace) → Metrics` and a function
`Objective(metrics, cycles, weights) → float`. Phase 6 uses both:

- `Objective` is the cost function the rewriter minimises.
- `Compute` over the trace corpus before and after a candidate rewrite
  is the equivalence check.

The optimisation loop is:

```
while True:
    candidates = generate_rewrite_candidates(live_graph, trace_corpus)
    for c in candidates:
        c_simulated = simulate(c, trace_corpus)
        c_cost     = Objective(Compute(c_simulated), cycles_of(c_simulated))
        c_equiv    = equivalent(simulate(current, trace_corpus), c_simulated)
        if c_cost < current_cost and c_equiv and scope_allowed(c):
            apply(c)   # emits the patch events
            current_cost = c_cost
            break
```

Reflex never adds a second metric, a second cost model, or a second
graph representation. The rewriter and the analyzer share the same
primitives. The composition is what makes the system tractable: the
hard problem (running the bus, computing metrics) was solved in
Phase 3; Phase 6 wires it backwards.

## Constraints on candidate generation

The candidate generator is bounded by the scope of the requesting
handler:

- archmotif with `mutate: [triage.*, chat.*]` can only generate
  candidates whose patches target handlers under those scopes.
- A patch that includes a `Subscribed` to an event type outside the
  requester's `read` scope is illegal — the enforcer will deny it.
- A patch that touches `core.*`, `system.*`, or `feedback.*` is
  forbidden regardless of `mutate` glob (the forbidden axis overrides).

This is what keeps the optimisation loop contained even when it's
running autonomously: the scope grants define the search space; the
forbidden zones are the hard floor; the human-gate handles
exceptions.

## What this gives the runtime

Once Phase 6 is in flight, reflex has:

- A live event-sourced topology (Phases 1–3).
- A live control plane that mutates by emitting events (Phase 4b).
- A permission model that scopes every mutation (Phase 4c).
- An optimiser that proposes rewrites within its scope, gated by the
  shared objective function (Phase 6).
- Human feedback that participates as ordinary rules, protected by
  scope guards from being silently rewritten (Phase 6 + 4c).

The system is observed, governed, and self-modifying — with every step
on the same bus, every change on the same log, and every constraint
declared in YAML or a `PermissionGranted` event. No layer is privileged;
no rewrite is invisible.
