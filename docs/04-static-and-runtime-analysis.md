# 04 — Static and runtime analysis

Two analysis layers ship today. Static analysis at config-load time
detects uncapped cycles via Tarjan SCC over the handler graph. Runtime
analysis post-drain (or `--watch`) reads the event trace and computes
causal-DAG metrics + a single objective scalar. Both will migrate to the
live subscription table in Phase 4b — a single source of truth that
removes the parsed-YAML / runtime drift entirely.

## Static analysis — load time

`pkg/graph` compiles a config into a `HandlerGraph` and runs validation
before the runtime accepts traffic. The compiler is intentionally
ignorant of how handlers actually run; it reads `HandlerSpec` (resolved
per-instance via `Registry.ResolveSpec`) and walks the introspection
projection.

### Nodes and edges

```go
type HandlerNode struct {
    Name     string        // unique handler instance name
    Type     string        // handler type
    Spec     HandlerSpec   // per-instance resolved spec
    LoopName string        // loop label, "" if not a loop node
    LoopCap  int           // max iterations, 0 if not a loop node
}

type HandlerEdge struct {
    From      string  // emitter handler name
    To        string  // consumer handler name
    EventType string  // the event type linking them
    Weight    int     // 1 by default; MaxIterations for edges out of loop nodes
    Terminal  bool    // emission flagged Terminal at the spec level
}
```

An edge exists from `N → M` for every `(EmittedSpec E in N.Spec.Emits,
node M with M.Spec.Consumes == E.Type)`. Edges are deterministic
(sorted by `from`, `to`, `event_type`) so textual output is reproducible.

### Cycle detection

The compiler runs Tarjan's SCC algorithm over the graph. Trivial SCCs
(single node, no self-loop) are dropped. A cycle is **capped** when at
least one node in the SCC declares `loop: { max_iterations: N }`.
Uncapped cycles produce a `CycleError` and the runtime refuses to start.

Tarjan's traversal ignores edges marked `Terminal` — a terminal
emission cannot spawn descendants by construction, so it cannot close
a runtime cycle even though it appears in the static graph. This is the
right behaviour: a graph like `A → B (terminal) → A` looks cyclic but
the dispatcher will never traverse the second edge.

### Per-instance spec resolution

The bulk of the cycle-detection precision comes from per-instance spec
resolution. Without it, `llm_stub` and `tool_call` would always appear
to form a cycle (`llm_stub` emits `ToolCallProposed`, `tool_call`
consumes it and emits `ToolResultObserved`, `llm_stub` consumes
`ToolResultObserved`…), even for a stub whose YAML rules only
`reply_and_handle`. The resolver scans the YAML config's rules +
fallback and narrows `Emits` to only the actions actually declared, so
the static graph is honest.

The same mechanism narrows `echo` (whose emit type is purely YAML-driven)
and `aggregator` (whose emit type is `config.emit`).

### Output

The graph is also the data source for `reflex describe`:

```
NAME       TYPE          DESCRIPTION              CONSUMES         EMITS                            LOOP
parse      parse_target  ...                      RequestReceived  TargetParsed, ParseFailed(T)     -
classify   triage_rules  ...                      GhQueryResult    TriageDecided, TriagePending(T)  -
bouncer    echo          ...                      PongEvent        PingEvent                        ping_pong(max=2)
...

7 handlers, 6 edges, 0 declared loops
```

`(T)` marks terminal emissions. The loop column shows the declared loop
label and cap when present.

### Loop caps as a side output

The compiled graph's `Caps()` method returns `map[handler_name]int` —
this is what `runtime.Build` passes to the bus via `bus.WithLoopCaps`,
closing the loop between the static declaration and runtime enforcement.

## Runtime analysis — Phase 3

`pkg/analyzer` is the background analyzer engine. It reads a JSONL
event trace produced by `reflex run --trace-file events.jsonl` (or
`reflex run --trace` piped through a JSON filter — the reader tolerates
mixed human/JSON output), constructs the causal DAG from the trace, and
computes graph metrics + a single objective scalar.

The analyzer is split into composable layers:

```
event.go            TraceEvent + JSONL reader
metrics.go          pure metric functions over a parsed trace
archmotif_adapter.go  bridge to archmotif's typed graph + power-diagonal cycle
objective.go        single-number objective the optimisation loop minimises
watch.go            directory-watch driver for the loop
analyzer.go         Analyze(trace) → Report
```

A trace is parsed into `[]TraceEvent`; `Compute(trace) → Metrics`
returns a bundle:

```go
type Metrics struct {
    PerRequest         map[string]RequestMetrics
    Orphans            []OrphanRecord
    HandlerUtilization map[string]int         // events emitted per source
    HandlerLatencyMS   map[string]float64     // median delta from trigger to emission
    TotalEvents        int
}

type RequestMetrics struct {
    CausalWidth          int      // max fan-out at any node in the request
    CausalDepth          int      // longest path from root to leaf
    TerminalCount        int
    TerminalTypes        []string
    TerminationCorrect   bool
    TerminationViolation string
}
```

### Termination correctness

Phase 1 invariant: every request must have at least one terminal that
closes the request (`RequestHandled`, `RequestUnhandled`, or
`TriagePending` — the last one is included because `triage_rules`
emits it as a legal "still waiting" closer). Other terminal events
(`GhQueryFailed`, `ParseFailed`, `EventOrphaned`, `LoopExhausted`) are
legal leaves but do not by themselves close the request.

`TerminationCorrect = false` when no closing terminal appears.
`TerminationViolation` carries the diagnostic.

### Orphan detection

The analyzer's orphan scan mirrors the runtime watcher: walk every
non-terminal event, flag those with no `caused_by` children. Phase 1
invariant says the slice should always be empty for a well-shaped
config. A non-empty result is a smoking-gun architectural violation
that any optimisation pass must correct (the objective function
penalises it heavily).

### Handler utilisation and latency

- **Utilisation**: events emitted per `source` (handler name). A handler
  with 0 emissions probably means a config-time wiring bug.
- **Latency**: median time-delta (ms) between a handler's trigger and
  the events it emits. The dispatcher is single-threaded, so latency is
  dominated by the slowest handler in the queue ahead of this one;
  outliers dominate a mean. Median is the honest "typical" number.

### Archmotif graph adapter

The analyzer builds an archmotif typed graph from the trace
(`pkg/analyzer/archmotif_adapter.go`):

- Each event maps to an archmotif `Package` node. The package id is the
  event id; `layer` carries the event type; `aggregate` carries the
  source handler.
- Each `caused_by` relationship maps to a `DependencyDependsOn` edge.

This is a forced fit — archmotif's node kinds are
Package / Type / Function / Method / Field, none of which are a
runtime-instance "event". `Package` is the only top-level kind exposed
via `archmotifimport.NewBuilder().AddPackage`; it works for the
adjacency algebra (which only cares about node IDs and edge endpoints)
but loses semantic fidelity. The adapter notes this for Phase 6/7
planning — a future archmotif extension exposing a generic
`NodeEvent` kind (or parametrising `AddPackage`'s display name) would
close the gap.

The current implementation also re-implements power-diagonal cycle
detection locally (`PowerDiagCycle`) because archmotif's matrix
validator framework (`PowerDiagOp`, `cycleMatrixInterpreter`) lives
under `internal/metrics` and is not reachable from outside archmotif.
The semantics match archmotif's `matrix_cycle` validator: for
`k ∈ [1..K]`, compute `A^k`; `(A^k)[i][i] > 0` means a closed walk of
length `k` touches node `i`.

The Phase 7 archmotif side-quest exposes `Encoder`, `Operation`,
`Interpreter`, `MatrixValidator`, and `LayerCollapseOp` from a public
`pkg/metrics` shim. Once that lands and a tagged release exists, the
analyzer can drop the local reimplementation and compose archmotif's
validators directly. See [`10-phase-roadmap.md`](./10-phase-roadmap.md).

On a healthy reflex trace `CyclingNodes` should be 0 — the event log is
a DAG by construction. A non-zero result is a hard invariant violation.

### Objective function

The Phase 3 brief: *minimise causal depth subject to no orphans + correct
termination*. The analyzer encodes this as a single scalar
(`pkg/analyzer/objective.go`):

```
Objective = max(per-request causal_depth)
          + 1000 * orphan_count
          + 1000 * mis-terminated_request_count
          + 1000 * cycling_node_count
```

Smaller is better. The 1000× penalty makes any architectural invariant
violation dwarf any plausible depth value, so "objective went down"
always means "either the graph stayed valid and got shallower, or a
violation was repaired". The penalty constants are exposed via
`ObjectiveWeights` so the JSON report is transparent and tests can
round-trip them.

Why max-depth and not mean-depth: reflex requests are independent; the
user-visible "how slow was the worst path" matters more than the
average. Why 1000 specifically: trace depths for the triage pipeline
are in the 3–5 range; one violation should dominate ~200
perfectly-shaped traces. A future phase running the optimiser over a
real workload should re-tune this constant.

### Output and watch mode

`reflex-analyzer --trace events.jsonl` prints a compact text summary:

```
trace:               events.jsonl
events:              19
requests:            1
objective:           4
max_causal_depth:    4
orphans:             0
cycling_nodes:       0
mis-terminated_reqs: 0

per-request:
  d4a9b8e2  width=2  depth=4  terminals=[RequestHandled, EventDispatched, DrainQuiesced]  ok

handler utilisation / latency:
  bus            events=6  median_latency_ms=0.20
  fetch_comments events=1  median_latency_ms=1.43
  ...
```

`--json` emits the full report as indented JSON.
`--watch ./traces/` re-analyses on each `.jsonl` file change and prints
the delta — the optimisation loop's read side.

`--metric objective` prints exactly one number for shell pipelines.

## Live-table cycle detection (Phase 4b)

Phase 4b promotes subscriptions to first-class events on the bus
(`HandlerRegistered`, `Subscribed`, `Unsubscribed`,
`HandlerDeregistered`, `SubscriptionRejected`). The bus owns a
`live_table` projection of those events — `bus.LiveTable()` returns the
current handler descriptors and subscription bindings — and the cycle
detector ports from "Tarjan over parsed YAML" to "Tarjan over the live
table".

Two checks now run:

1. **YAML pre-flight (defence in depth).** `graph.Build` still reads the
   parsed config and refuses to return a bus when the static graph has
   an uncapped cycle. This is the fast path — there's no point
   queueing seed events when the YAML itself is broken.
2. **Live-table authority.** After every `Subscribed` arrives — at boot
   from the YAML seed stream or at runtime via `bus.SubscribeWithCheck`
   — the bus reruns Tarjan over the live subscription table. A new
   subscription that would close an uncapped cycle is rejected: the
   bus does NOT add the binding and emits
   `SubscriptionRejected{handler_name, event_type, reason}`. The
   `SubscribeWithCheck` call returns a non-nil error to the caller.

The runtime `Build` call also runs `bus.CheckLiveTableCycles()` after
all YAML handlers have been registered, so any drift between the
parsed-YAML pre-check and the actual descriptors that reached the bus
is caught before any user event flows.

Algorithmic notes:

- The adjacency rules are unchanged: for every `(emitter handler H
  emits T)` × `(subscriber handler S consumes T)`, edge H→S exists.
  Terminal emissions are excluded — a terminal cannot spawn a
  descendant by construction.
- A cycle is "capped" iff at least one handler in the SCC has a
  subscription with `MaxIterations > 0`. The set of caps is sourced
  from the same `WithLoopCaps` map the dispatcher enforces, so the
  static check and the runtime cap enforcement stay in lockstep.

The Phase 3 analyzer's static graph adapter is unaffected — it still
operates on the YAML-derived `HandlerGraph`. A future refactor could
have the analyzer query `bus.LiveTable()` directly when running over a
live daemon, but the offline `--trace` workflow has no live bus to
query and stays as-is. See
[`05-control-plane-as-events.md`](./05-control-plane-as-events.md) for
the full control-plane shape.

## The archmotif side-quest

Reflex's analyzer composes archmotif's matrix validators when archmotif
exposes them publicly. The required surface is small:

```
pkg/metrics.Encoder          // graph → matrix
pkg/metrics.Operation        // pure matrix-level op
pkg/metrics.Interpreter      // matrix → diagnostic
pkg/metrics.MatrixValidator  // Encoder + Operation + Interpreter
pkg/metrics.LayerCollapseOp  // collapse strongly-coupled subgraph into one node
```

Once a tagged archmotif release exposes these, the analyzer:

1. Drops `PowerDiagCycle` in favour of `metrics.MatrixCycleValidator`
   composed against the trace graph.
2. Gains `LayerCollapseOp` for the Phase 6 compression pass that merges
   strongly-coupled subgraphs into a single composite handler.

This is Phase 4d in the roadmap. The `go.mod` `replace` directive
currently pointing at the local archmotif checkout disappears at the
same time.
