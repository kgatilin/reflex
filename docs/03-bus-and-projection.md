# 03 — Bus and projection

The bus contract: publish, subscribe, drain. The bus's meta-events. The
projection store as a per-request side-channel. The generic aggregator
pattern. The CLI wait-predicates that sit over all of this.

## Bus contract

The bus is a single in-memory dispatcher (`pkg/bus/bus.go`). Its public
surface is small:

```go
b := bus.New(store,
    bus.WithSource("cli"),
    bus.WithMaxSteps(256),
    bus.WithProjection(projStore),
    bus.WithLoopCaps(g.Caps()),
)
b.Register(handler1)
b.Register(handler2)
// ...
b.WireProjection()         // inject the projection into ProjectionAware handlers
err := b.Run(ctx, seed)    // drain
```

`Run` appends the seed event to the store, fans out to matching
subscribers, appends any events they return, and repeats until the queue
is empty (quiescence) or `max_steps` is hit. The drain is a plain loop,
not a goroutine pool: reflex wants deterministic ordering and a clean
trace.

A subscriber is a pure reactor:

```go
type Subscriber interface {
    Name() string
    Match(ev event.Event) bool
    React(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error)
}
```

`Match` decides interest; `React` is called only when `Match` returned
true. `React` receives a snapshot of the log up to and including the
trigger event, and returns zero or more new events. It must not mutate
the store directly — anything it needs to track between calls it derives
from the log (or from the projection store, for structured cached
results).

`Match` is called exactly once per event per subscriber. Subscribers
that match multiple types implement `Match` as a type-set check
(e.g. the aggregator matches both its consumed response type and the
fan-out `EventDispatched`).

### Fan-out semantics

When the dispatcher pops an event, it snapshots the store, then walks
the subscriber list in registration order. All matching subscribers see
the same log snapshot — new events appended by earlier subscribers in
the fan-out are not visible to later ones at the same depth. They appear
on the next dispatcher iteration.

This is what keeps causal ordering clean. Sibling reactions cannot
observe each other; only descendants do. The trace remains a clean DAG
rooted at the seed event.

### Loop-cap enforcement

For every subscriber whose `Name` is keyed in `loopCaps`, the dispatcher
tracks a per-`(request_id, handler_name)` fire count. When firing one
more time would exceed the cap, the subscriber is *not* called: the
dispatcher emits a terminal `LoopExhausted{handler, max_iterations,
reason}` event in its place, `caused_by` the trigger event. The terminal
flag closes the causal branch so the orphan watcher stays silent.

This is the runtime side of Phase 1.5's static cycle-cap declaration —
the YAML declares which handler bears the cap, and the dispatcher
enforces it.

### Failure semantics

If `React` returns an error, the dispatcher:

1. Emits a terminal `HandlerFailed{handler_name, event_type, error}` to
   the store (so the trace records the failure even if the caller treats
   the error as fatal).
2. Returns the error from `Run`. The CLI surfaces it as an exit-2 with
   the failure message.

The error path does not attempt recovery. A handler that wants to be
fault-tolerant catches its own errors and emits a domain event
(`GhQueryFailed`, `ParseFailed`, …) rather than returning err.

## Bus meta-events

The bus emits three meta-events about its own activity, all terminal:

```
EventDispatched{event_type, subscriber_count}
DrainQuiesced{request_id}
HandlerFailed{handler_name, event_type, error}
```

### EventDispatched

After the bus has finished delivering an event to all matching
subscribers, it emits one `EventDispatched` event with:

- `event_type` — the type of the trigger event.
- `subscriber_count` — the number of subscribers whose `Match` returned
  true and whose `React` was called (loop-capped fires excluded).

`EventDispatched` is the foundation for the aggregator pattern: a
subscriber that wants to know "how many parallel paths are about to
fire?" subscribes to `EventDispatched{event_type: <fan-out type>}` and
reads `subscriber_count` from the payload. No advance configuration of
fan-out width is needed.

The bus never emits an `EventDispatched` about another meta-event —
that would recurse. The orphan watcher excludes `EventDispatched` from
its descendant counts so a non-terminal event still surfaces as an
orphan when nothing genuine reacted to it.

### DrainQuiesced

After the queue empties, the dispatcher emits one `DrainQuiesced` per
`request_id` it observed during the run. `DrainQuiesced` is the
canonical "this request is done" signal — it fires whether or not the
request closed cleanly (`RequestHandled` vs. `RequestUnhandled`).

The CLI `--wait drain` predicate keys off this.

### HandlerFailed

Emitted when a handler's `React` raises. Carries handler name, the event
type that triggered the failure, and the error string. The dispatcher
emits it before returning the error from `Run`, so the trace always
contains a record of the failure (even if subsequent processing is
aborted).

The unhandled watcher is not invoked after a `HandlerFailed` — the run
is aborted; no quiescence check runs.

## Projection store

`SessionProjection(events, request_id)` is the canonical state
derivation: a pure fold of the log into a `SessionState` struct
(`pkg/projection/projection.go`). The projection cannot be written to
from inside a handler — the log is reality.

Some patterns nonetheless want to stash a structured intermediate result
that downstream handlers pick up by key. The `projection.Store`
(`pkg/projection/store.go`) is that side-channel:

```go
s := projection.NewStore()

s.Set(requestID, "triage.verdict", "STUCK")
v, ok := s.Get(requestID, "triage.verdict")    // v = "STUCK", ok = true
present := s.Has(requestID, "triage.verdict")  // true

snap := s.Snapshot()        // map[requestID]map[key]value, for trace output
ids := s.RequestIDs()       // sorted list
m := s.ForRequest(requestID)
```

The store is per-`reflex run`, in-memory, mutable, and JSON-marshalable
(it implements `json.Marshaler`). It is `sync.RWMutex`-guarded so
concurrent handlers can read while one writes. Nil-safe: passing a `nil`
store to `Set` / `Get` / `Has` is a no-op, so handlers don't have to
nil-check.

### ProjectionAware

Handlers that want the store inject it at construction time via the
`bus.ProjectionAware` interface:

```go
type ProjectionAware interface {
    SetProjection(p *projection.Store)
}
```

The bus calls `WireProjection()` after the subscriber list is assembled;
that walks every registered subscriber and calls `SetProjection` on the
ones that implement the interface. Handlers that don't implement
`ProjectionAware` never see the store and don't need to know it exists.

### When to use store vs. event

The rule: anything that should affect causal structure stays an event.
The store is for cached projection material — "I have decided X and
don't want to re-emit every time someone asks". `triage_rules` stashes
its verdict under `triage.verdict` so the CLI's
`projection.has=triage.verdict` predicate can wait for it without
needing a per-classifier event type to subscribe to.

The store does not replace the log. Crashing and rebuilding the store
from the log is always possible (call `SessionProjection` for each
request_id, derive whatever the previous handlers would have stashed).
The store is purely a performance optimisation over re-folding.

## Generic aggregator pattern

The generic aggregator (`pkg/handler/aggregator.go`) is the canonical
fan-out / barrier:

```yaml
- name: collect
  type: aggregator
  on:    Classification          # the per-handler response events
  emits: [ClassificationsAggregated]
  config:
    expected_from: ClassifyRequested        # learn count from EventDispatched of this type
    emit:          ClassificationsAggregated # the aggregated event type (terminal)
```

It subscribes to two event types: its target response type
(`Classification`) and `EventDispatched`. On `EventDispatched` with
`event_type == ac.ExpectedFrom`, it records `subscriber_count` as the
expected count for the request. On each response, it appends the
payload. Once the accumulated count reaches the expected count (and the
aggregated event hasn't already been emitted for this request), it emits
the aggregated event:

```json
{
  "items": [<response payload 1>, <response payload 2>, <response payload 3>],
  "count": 3
}
```

The aggregated event is terminal — the aggregator is its own causal arm
closer.

Per-request bookkeeping (received items, expected count, fired flag) is
kept in the handler instance under a mutex. The aggregator is one of the
few stateful handlers; its state is cleanly recoverable from the log
(re-fold response events + the relevant `EventDispatched`) so it
satisfies the "log is reality" rule.

### Why this avoids a hook

A naive implementation might do `await Promise.all(classifiers.map(...))`.
Reflex cannot: there is no synchronous primitive. But the aggregator
gives the same semantics: the per-classifier responses fire in any order,
the aggregator collects them, and the aggregated event fires once. The
"barrier" is a property of the aggregator's accumulated state, not a
property of the bus.

The cost is one extra meta-event per fan-out (`EventDispatched`), which
the bus emits anyway as part of its self-hosting. The benefit is that
the aggregator is just a handler: it can be reused across patterns,
introspected like any other, swapped for a different aggregation
strategy, or composed with other aggregators without special framework
support.

## CLI wait-predicates

`reflex emit / invoke / send` all accept `--wait <predicate>` and event
configs can declare a default predicate under `events:[].cli.wait`. Three
predicates ship today (`cmd/reflex/main.go` `checkWaitPredicate`):

```
--wait drain
```

succeeds once `DrainQuiesced` has fired for the request's request_id.
This is the default for fire-and-forget events.

```
--wait request_id_terminal
```

succeeds once any *user-domain* terminal event has fired for the
request's request_id. Meta-events (`EventDispatched`, `DrainQuiesced`,
`HandlerFailed`) explicitly don't count — they describe the bus, not
user-domain completion. Use this when "the request reached an explicit
leaf" is the success condition.

```
--wait projection.has=<key>
```

succeeds once the projection store contains `key` for the request's
request_id. Use this when a specific handler is expected to have stashed
its decision.

The current CLI evaluates predicates post-drain (the bus drains, then
the predicate is checked). The Phase 4a daemon will run predicates
mid-drain: the HTTP/Go embed API will block on a channel that resolves
when the predicate first becomes true, regardless of whether the drain
has finished or other events for other requests are still in flight.

Wait-predicates are not a privileged construct. They are validators over
the (log ∪ projection) state. New predicates can be added by extending
the same `checkWaitPredicate` switch.

## Why drain rather than push-based

A subscriber that wants to react asynchronously to "the drain quiesced"
can subscribe to `DrainQuiesced`. A subscriber that wants to react to
"the bus is doing too much" can subscribe to `EventDispatched` and
count. A subscriber that wants to take action on a handler failure can
subscribe to `HandlerFailed`. All three are first-class events; no
out-of-band hook is provided or needed.

This is the property that lets archmotif (Phase 7) sit inside the bus
as a subscriber. The runtime graph it maintains is a projection over
the control-plane and meta-event streams. The compression cycle it
drives is an ordinary chain of emit / receive on the same bus. See
[`07-archmotif-as-live-subscriber.md`](./07-archmotif-as-live-subscriber.md).
