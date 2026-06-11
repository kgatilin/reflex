# 02 — Handlers and schemas

The YAML handler grammar, the self-describing-handler contract, event
payload conventions, and the terminal-event invariant. Together these
define the static surface a reflex config presents to the runtime, the
graph builder, and the introspection tools.

## YAML grammar

A reflex config is a flat list of handlers plus optional sections for
event declarations and settings. The minimal form:

```yaml
settings:
  max_steps: 64           # optional bound on dispatcher iterations per run

events:                   # optional Phase 1.6 section (CLI bindings)
  - name: RequestReceived
    args: { payload: string }
    cli:
      command: invoke triage
      wait:    projection.has=triage.verdict

handlers:
  - name: parse           # required, unique label, appears in the trace
    type: parse_target    # required, must be a registered handler type
    on:   RequestReceived # required, the event type the handler subscribes to
    emits: [TargetParsed, ParseFailed]   # informational
    config: { default_owner: kgatilin }  # handler-specific parameters
    loop:                                # optional, declares cycle-cap node
      max_iterations: 2
      name: review_loop                  # optional label, defaults to handler name
```

`name` is the handler's unique instance label. The dispatcher uses it as
the handler's `Source` attribution on every emitted event; the loop-cap
enforcement is keyed by it. `type` selects the registered factory + spec
in `pkg/handler`. `on` is the consumed event type (handlers whose
`Spec.Consumes == "*"` get their consumed type from `on:` at config-load
time). `emits` is informational only — the static graph uses the
handler's `HandlerSpec.Emits` set, not the YAML — but it is the right
place to document what readers should expect.

`config` is an opaque map passed to the factory. Each handler type
defines its own schema for it. `loop` is the Phase 1.5 declaration that
this handler is the cap-bearing node of a cycle; `max_iterations` must be
> 0.

### Aggregator-specific grammar

```yaml
- name: collect
  type: aggregator
  on:    Classification               # the per-handler response events
  emits: [ClassificationsAggregated]  # informational
  config:
    expected_from: ClassifyRequested  # subscriber_count comes from EventDispatched of this type
    emit:          ClassificationsAggregated   # the aggregated event type (terminal)
```

`expected_from` is the fan-out trigger event type; the aggregator reads
`EventDispatched{event_type: ClassifyRequested}.subscriber_count` to learn
how many responses to wait for. `emit` is the aggregated event type. See
[`03-bus-and-projection.md`](./03-bus-and-projection.md) for the full
pattern.

### Loop-specific grammar

```yaml
- name: bouncer
  type: echo
  on:   PongEvent
  config: { emit: PingEvent }
  loop:
    max_iterations: 2     # fire at most twice per request
    name: ping_pong       # optional disjoint-loop label
```

When `loop:` is present the dispatcher tracks `(request_id, handler_name)`
fire counts and refuses to fire past `max_iterations`. On the iteration
that would exceed the cap it emits a terminal
`LoopExhausted{handler, max_iterations, reason}` instead of calling the
handler again. The terminal flag closes the causal branch so the orphan
watcher stays silent.

The static graph builder (`pkg/graph/graph.go`) requires every cycle to
be capped: at least one node in each strongly-connected component must
declare `loop:`. Uncapped cycles are a hard error and the runtime refuses
to start.

## Self-describing handlers

Every registered handler type ships with a `HandlerSpec`
(`pkg/handler/handler.go`):

```go
type HandlerSpec struct {
    Type        string        // YAML `type:` discriminator
    Description string        // single-sentence human-readable
    Consumes    string        // event type subscribed to; "*" if dynamic-from-config
    Emits       []EmittedSpec
}

type EmittedSpec struct {
    Type     string
    Terminal bool   // closes the causal branch
    Optional bool   // emission depends on input
}
```

The `Registry` keeps `HandlerSpec` alongside the factory; the
`Introspect` projection (`ListTypes`, `SpecOf`, `Emitters`, `Consumers`)
gives downstream code (graph builder, validate CLI, describe CLI, future
daemon) a read-only view of the topology without instantiating any
handlers.

### Spec resolvers

Some handler types have emission sets that depend on YAML config — `echo`
emits whatever is in `config.emit`; `llm_stub` emits the union of actions
declared in its rules + fallback. The registry supports an optional
`SpecResolver(cfg, base) → resolved` per type. The graph builder calls
`Registry.ResolveSpec(cfg)` instead of the raw `SpecOf(type)`, so the
static graph reflects per-instance emissions rather than the type-level
maximum.

This matters for cycle detection: a stub configured to only
`reply_and_handle` is not a `ToolCallProposed` emitter, so its edge to
`tool_call` does not exist, and the spurious `llm_stub ↔ tool_call`
cycle for `examples/calc.yaml` does not appear in the graph.

### Built-in handler types

The current built-in registry (`pkg/handler.BuiltinRegistry`) ships:

| `type:`              | Consumes               | Emits                                                 |
|----------------------|------------------------|-------------------------------------------------------|
| `llm_stub`           | configurable (`on:`)   | `ToolCallProposed`, `AssistantMessageProposed`, `RequestHandled` |
| `tool_call`          | `ToolCallProposed`     | `ToolResultObserved`                                  |
| `printer`            | configurable           | (sink — no emissions)                                 |
| `terminator`         | configurable           | `RequestHandled` (terminal)                           |
| `unhandled_watcher`  | `__noop__`             | `RequestUnhandled`, `EventOrphaned` (post-drain)      |
| `echo`               | configurable           | configured emit type                                  |
| `parse_target`       | configurable           | `TargetParsed`, `ParseFailed` (terminal)              |
| `gh_query`           | configurable           | `GhQueryResult`, `GhQueryFailed` (terminal)           |
| `triage_rules`       | configurable           | `TriageDecided`, `TriagePending` (terminal)           |
| `aggregator`         | configurable           | configured emit type (terminal)                       |

The introspection contract lets `reflex describe --config <yaml>` render
this table for any config without running it.

## Event schema

Every event on the log has the same shape (`pkg/event/event.go`):

```go
type Event struct {
    ID        string          // UUID, dispatcher-assigned
    Type      string          // discriminator (e.g. "RequestReceived")
    RequestID string          // groups events for one user request
    TS        time.Time       // dispatcher-assigned
    Source    string          // emitting handler's Name
    CausedBy  string          // ID of the event that triggered this one
    Terminal  bool            // explicit leaf of the causal DAG
    Payload   json.RawMessage // opaque JSON, schema set by handlers
}
```

The dispatcher assigns `ID`, `TS`, and (when omitted) `Source` and
`CausedBy`. Handlers fill `Type`, `Terminal`, and `Payload`. `RequestID`
propagates from the triggering event automatically.

`caused_by` is the only causal pointer. Reconstructing the causal DAG is
a single pass: `parent.id → []children`. The analyzer relies on this
(`pkg/analyzer/metrics.go`).

### Payload conventions

Payloads are opaque JSON to the framework. Handlers that produce or
consume an event type agree on its payload shape. The convention used by
the built-in handlers:

```jsonc
// RequestReceived
{ "payload": "<user message>" }

// TargetParsed
{ "owner": "kgatilin", "repo": "archai", "number": 114 }

// GhQueryResult
{ "path": "comments", "data": [...] }

// TriageDecided
{ "classification": "STUCK", "reason": "label_age=267h, kira=0 → STUCK" }

// AssistantMessageProposed
{ "text": "The answer is 4" }

// ClassifyRequested (fan-out trigger)
{ "item": "foo" }

// EventDispatched (meta)
{ "event_type": "ClassifyRequested", "subscriber_count": 3 }

// DrainQuiesced (meta)
{ "request_id": "<uuid>" }

// HandlerFailed (meta)
{ "handler_name": "fetch_comments", "event_type": "TargetParsed", "error": "..." }

// LoopExhausted
{ "handler": "bouncer", "max_iterations": 2, "reason": "loop cap reached" }
```

`request_id` lives on the envelope rather than the payload because every
event has one. Payloads carry only the data the consuming handler needs.

## The terminal-event invariant

Reflex enforces a Phase 1 architectural invariant: every non-terminal
event must have at least one descendant. The intuition is that a handler
that emits a non-terminal event is making a claim ("something else will
happen because of this"); if drain finishes and nothing happened, that
claim was wrong.

The post-drain orphan check (`pkg/handler/unhandled_watcher.go`
`CheckQuiescence`) walks the snapshot, counts children for each event
via `caused_by`, and emits a terminal
`EventOrphaned{orphan_id, orphan_type, request_id, reason}` for every
non-terminal event with zero descendants. `EventOrphaned` is a hard
diagnostic — an architectural-violation flag, distinct from the
request-level `RequestUnhandled`.

The orphan scan deliberately skips `EventDispatched` as a child counter:
otherwise every routed event would have at least one "child" by
construction and no genuine orphan would ever surface. `LoopExhausted`
and `DrainQuiesced` are handled in the obvious way (the former counts as
the descendant the trigger needed; the latter has no `caused_by`).

### Handler responsibilities

A handler that emits a non-terminal event implicitly promises a
descendant. The built-in handlers obey this:

- `llm_stub` action `reply_and_handle` emits both
  `AssistantMessageProposed` (terminal) and `RequestHandled` (terminal),
  closing both arms.
- `terminator` emits `RequestHandled` (terminal).
- `triage_rules` emits `TriageDecided` (non-terminal — followed by
  printer + terminator) on the success path and `TriagePending`
  (terminal) on the "waiting for the other branch" path. The
  `TriagePending` terminal explicitly closes the trigger
  `GhQueryResult`'s causal arm so the invariant holds even when only one
  branch has arrived at the time `triage_rules` fires.
- `parse_target` failure → `ParseFailed` (terminal).
- `gh_query` failure → `GhQueryFailed` (terminal).

Custom handlers default to non-terminal (`event.New(...)`) and opt into
terminal only for genuine leaves (`event.NewTerminal(...)`).

## Validate and describe

The CLI exposes two introspection commands:

```
reflex validate --config <yaml>
# config valid: N handlers, M edges, K declared loops
# (or: cycle detected: <a> -> <b> -> <a>; no max_iterations declared; refusing to start)

reflex describe --config <yaml>
# NAME       TYPE          DESCRIPTION              CONSUMES         EMITS                            LOOP
# parse      parse_target  ...                      RequestReceived  TargetParsed, ParseFailed(T)     -
# bouncer    echo          ...                      PongEvent        PingEvent                        ping_pong(max=2)
# ...
```

Both run against the introspection projection plus the YAML; neither
instantiates a single handler. `describe` works on a cyclic config too —
humans inspect broken topologies more often than healthy ones.

The same projection feeds the static cycle detector (Phase 1.5) and the
runtime analyzer (Phase 3). See
[`04-static-and-runtime-analysis.md`](./04-static-and-runtime-analysis.md).

## Why self-description matters

When Phase 4 promotes the runtime to a daemon with multi-process
handlers (`pkg/embed`, HTTP, optional gRPC), every remote handler will
announce itself to the bus with exactly this `HandlerSpec` shape. The
daemon will run the same graph validation before accepting the handler
into the live subscription table. There is no second schema for "remote
handlers" — the introspection contract is the wire format. Phase 4b
formalises this by promoting `HandlerRegistered` to a first-class
event with the spec embedded in its payload; see
[`05-control-plane-as-events.md`](./05-control-plane-as-events.md).
