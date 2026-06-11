# 07 — archmotif as live subscriber

Phase 7 vision. archmotif is not a static analyser CLI invoked over a
trace file; it is a bus-resident handler. It subscribes to control-plane
and meta-event streams, maintains the runtime graph as a projection,
detects pathologies live, and drives the compression cycle by emitting
ordinary events. It has no privileged access — its scope is declared
like any other handler.

## The shift

Phase 3 archmotif (today, `pkg/analyzer`) reads a JSONL trace file
post-drain, builds a graph, runs validators, and prints a report. It is
external to the live system.

Phase 7 archmotif is inside the bus. It subscribes to the same events
every other handler does, holds its graph as a projection, reacts to
control-plane changes the moment they happen, and proposes
modifications back to the same bus. The boundary between "static
analyser" and "live participant" disappears.

The shape of the analysis does not change — same metrics, same matrix
validators (composed from archmotif's `pkg/metrics` shim once the
Phase 4d side-quest lands), same objective scalar. What changes is the
input source (live event stream instead of file) and the output
(`CompressionRequested` / `Subscribed` / `Unsubscribed` events instead
of stdout).

## Subscriptions

```yaml
- name: archmotif
  type: archmotif_subscriber
  on: [
    # Control-plane: maintain the live graph projection
    HandlerRegistered,
    HandlerDeregistered,
    Subscribed,
    Unsubscribed,
    # Meta: observe dispatch and failure
    EventDispatched,
    DrainQuiesced,
    HandlerFailed,
    # Compression cycle responses
    CompressionContext,
    CompressionAccepted,
    CompressionRejected,
  ]
  scope:
    mutate:    [analytics.*, triage.*]
    read:      [*]
    forbidden: [core.*, system.*, feedback.*]
    meta:
      grant:   [analytics.*]
  config:
    trace_window:        1000      # events to keep in the rolling window
    pathology_threshold:
      orphan_count:      1
      cycle_depth:       8
      latency_p95_ms:    500
```

The subscription set is large but uniform. archmotif sees every event
that bears on graph state: registration, subscription change,
dispatch, failure. It does not subscribe to domain payloads it doesn't
need; the live graph projection is built from event types and source
attributions, not from message bodies.

## The runtime graph projection

archmotif maintains an in-memory `RuntimeGraph` updated on every
control-plane and meta-event:

- `HandlerRegistered` adds a node with the declared spec.
- `HandlerDeregistered` removes the node and any edges incident on it.
- `Subscribed` may add an edge from every node whose `Emits` includes
  the new event type.
- `Unsubscribed` removes those edges.
- `EventDispatched` increments the firing-count on the matched edges
  (the trace-corpus data the compression heuristics need).
- `HandlerFailed` increments the failure count on the implicated handler
  (drives the "rogue handler" detector).

The graph is a projection over the bus log — losing it is recoverable
by re-folding the relevant event streams. archmotif holds the
projection in memory for query performance, not as authoritative
storage.

The same projection feeds the existing Phase 3 metrics
(`pkg/analyzer/metrics.go` ports to operate on the live graph instead
of a trace file): `CausalDepth`, `CausalWidth`, `Orphans`,
`HandlerUtilization`, `HandlerLatencyMS`. The trace window
configuration (`trace_window: 1000`) bounds the rolling sample.

## Pathology detection

archmotif fires on `DrainQuiesced` for each request: recompute the
relevant metrics over the window, check against thresholds, emit a
`CompressionRequested` event when a threshold is crossed.

```jsonc
// CompressionRequested
{
  "target":   ["triage.classifier", "triage.aggregator"],
  "reason":   "causal_depth=6 > target=4; pass-through compression candidate",
  "evidence": {
    "trace_window": 1000,
    "p95_depth":    6,
    "median_latency_ms": 320,
    "matrix_op":    "LayerCollapseOp",
    "objective":    1006.0
  },
  "request_id": "<the request that triggered the threshold>"
}
```

`target` names the candidate nodes for rewrite. `evidence` carries the
data the heuristic acted on so other handlers (the human-gate, the
audit logger, a future explainability subscriber) can inspect the
reasoning. The reason field is a short human-readable diagnosis;
clients should not parse it.

`CompressionRequested` is **not terminal**. It expects a response — the
compression cycle below.

## The compression cycle

The compression cycle is a coordinated emit / receive chain on the bus.
No new primitives; the pattern uses
[`03-bus-and-projection.md`](./03-bus-and-projection.md)'s aggregator
plus an acceptance handler.

```
1. archmotif emits CompressionRequested{target, reason, evidence}.

2. The bus fans out to every handler in target. Each is expected to
   respond with CompressionContext{state, recent_invocations,
   constraints} — describing what would be lost / gained / constrained
   by the proposed change.

3. An aggregator (collect_compression_context) collects the
   CompressionContext events, keyed by the CompressionRequested's id
   (it reads EventDispatched.subscriber_count for the
   CompressionRequested fan-out to learn how many to expect).

4. The aggregator emits CompressionContextAggregated{contexts: [...]}.

5. archmotif consumes CompressionContextAggregated and computes the
   actual rewrite patch:

   CompressionProposed{
     patches: [
       {op: "Subscribed",   handler: "triage.merged", event_type: "..."},
       {op: "Unsubscribed", handler: "triage.classifier", event_type: "..."},
       {op: "Unsubscribed", handler: "triage.aggregator", event_type: "..."},
       {op: "HandlerRegistered", name: "triage.merged", consumes: "...", emits: [...]},
       {op: "HandlerDeregistered", name: "triage.classifier"},
       {op: "HandlerDeregistered", name: "triage.aggregator"},
     ],
     confidence: 0.82,
     objective_delta: -3.0
   }

6. The acceptance handler decides:
     - confidence > threshold && auto_apply: emit CompressionAccepted
       and publish the patch events.
     - otherwise: emit HumanGateRequested{patch} for review.

7. On CompressionAccepted, the patch's Subscribed / Unsubscribed /
   HandlerRegistered events are published, and the live table updates.
   The cycle detector and audit logger see them like any other
   control-plane event.

8. archmotif's runtime graph projection updates from those events
   automatically — same subscription it uses for state maintenance.
```

The cycle closes when the rewrite has been applied (or rejected). The
next `DrainQuiesced` will re-evaluate metrics against the updated
topology.

### What makes this work

The compression cycle is not a special-cased framework feature. It is
a normal event chain:

- `CompressionRequested` is just an event type with a documented
  payload shape.
- The aggregator is the same `pkg/handler/aggregator.go` used for the
  3-classifier example, parameterised to collect `CompressionContext`
  events.
- The patch application uses `Subscribed` / `Unsubscribed` /
  `HandlerRegistered` / `HandlerDeregistered` — the same control-plane
  events covered in [`05-control-plane-as-events.md`](./05-control-plane-as-events.md).
- The acceptance handler is a normal handler; deployments swap the
  auto-acceptance variant for the human-gate variant by changing one
  line of YAML.

archmotif sits in the bus with declared scope (`mutate: [analytics.*,
triage.*]`). Its patches that target handlers outside its scope are
caught by the enforcer and surfaced as `PermissionDenied` — archmotif
sees the denial and re-plans (or escalates).

## Scope as containment

archmotif's `forbidden: [core.*, system.*, feedback.*]` is the key
containment. The optimiser cannot rewrite the basic event flow, cannot
touch bus internals, and cannot silently merge or remove handlers a
human added under `feedback.*`. The forbidden axis overrides any
broader `mutate` glob.

This composes with the Phase 4c permission events: archmotif can grant
sub-handlers under `analytics.*` (its `meta.grant` allows it) but
cannot mint a grant for `core.*` (its `meta.grant` does not include it).
The enforcer catches the violation and emits `PermissionDenied`.

## Use of archmotif's pkg/metrics

Once the archmotif side-quest lands (Phase 4d), archmotif's
`pkg/metrics` exposes:

```
Encoder           graph → matrix
Operation         pure matrix-level transformation
Interpreter       matrix → diagnostic
MatrixValidator   Encoder + Operation + Interpreter
LayerCollapseOp   collapse strongly-coupled subgraph into one node
```

archmotif inside reflex then composes these directly. The
`PowerDiagCycle` re-implementation in
`pkg/analyzer/archmotif_adapter.go` disappears in favour of
`metrics.MatrixCycleValidator`. The `LayerCollapseOp` becomes the
heuristic that powers the cluster-collapse compression pass
(see [`08-optimization-as-rewrite.md`](./08-optimization-as-rewrite.md)).

The composition is what makes archmotif a small handler rather than a
large analyser: its job inside the bus is *subscription bookkeeping
plus pattern-matching against trace metrics*; the matrix algebra lives
in `pkg/metrics`.

## Observability

Because archmotif is a handler, its outputs are events. A debug
configuration that wants to see archmotif's thinking subscribes to
`CompressionRequested`, `CompressionContext`,
`CompressionContextAggregated`, `CompressionProposed`,
`CompressionAccepted` / `CompressionRejected`, and writes them to a
sink. The full reasoning chain is on the bus log; no archmotif-internal
logging is needed for observability.

The audit logger of [`06-permissions-and-scopes.md`](./06-permissions-and-scopes.md)
already subscribes to control-plane and permission events, so any
`PermissionDenied` archmotif triggers is recorded automatically. The
analyser of [`04-static-and-runtime-analysis.md`](./04-static-and-runtime-analysis.md)
already reads `DrainQuiesced` and computes the objective, so the
post-rewrite delta surfaces in the metrics stream the same way.

## Why archmotif is not a privileged plane

The temptation is to give archmotif (or any "optimiser") direct access
to a runtime data structure — "let it call `bus.RewriteSubscriptions()`
synchronously". Reflex doesn't:

- The control-plane events ARE the rewrite primitive. archmotif emits
  them; the bus dispatches them; the live table updates; subscribers
  observe.
- The enforcer checks every event uniformly. archmotif gets no special
  treatment.
- The audit log records every change. There is no out-of-band rewrite
  archmotif could perform that the audit log would miss.
- The cycle detector reacts to the same events. archmotif cannot
  smuggle in a topology that bypasses validation.

This is the same property that makes the permission enforcer viable as
a handler: every component reads and writes the same bus, with declared
scope, and the system's behaviour is the closure of those declarations.
No backdoor; no privileged primitives; just events.
