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
(`StepFetchFailed`, `RequestUnhandled`, …) rather than returning err.

## Bus meta-events

The bus emits two classes of meta-events about its own activity.

### Dispatch meta-events (Phase 1.6), all terminal:

```
EventDispatched{event_type, subscriber_count}
DrainQuiesced{request_id}
HandlerFailed{handler_name, event_type, error}
```

### Control-plane events (Phase 4b), all terminal:

```
HandlerRegistered{name, consumes, emits, description}
Subscribed{handler_name, event_type, max_iterations?}
Unsubscribed{handler_name, event_type}
HandlerDeregistered{handler_name}
SubscriptionRejected{handler_name, event_type, reason}
```

`HandlerRegistered` + `Subscribed` are emitted by `Register`/`SubscribeWithCheck`;
`Unsubscribed` + `HandlerDeregistered` are emitted by `Unsubscribe`/`HandlerDeregister`.
`SubscriptionRejected` is emitted when a runtime subscription would
introduce an uncapped cycle in the live subscription table — the
subscription does NOT take effect and the offending caller receives a
non-nil error from `SubscribeWithCheck`.

Control-plane events emitted outside an active `Run` (e.g. at config-load
time) are queued and delivered to subscribers on the next `Run` so audit
handlers can react to boot-time topology even though no domain event has
fired yet. They are excluded from the `EventDispatched` meta-event class
so registering N handlers doesn't produce N×EventDispatched noise.

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

s.Set(requestID, "calc.verdict", "4")
v, ok := s.Get(requestID, "calc.verdict")      // v = "4", ok = true
present := s.Has(requestID, "calc.verdict")    // true

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
don't want to re-emit every time someone asks". A decide handler stashes
its verdict under `calc.verdict` so the CLI's
`projection.has=calc.verdict` predicate can wait for it without
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

## Phase 4a — bus as a daemon, handlers over a socket

Phase 4a moves the bus into a long-running process that exposes the same
pub/sub semantics over a Unix domain socket. The wire protocol is
newline-delimited JSON with one connection per handler.

The shape:

```
reflex daemon --config examples/aggregate.yaml \
    --socket /tmp/reflex-demo.sock
# (terminal stays attached; SIGINT/SIGTERM cleanly drain + close)
```

YAML-declared handlers are loaded into the daemon's bus at startup, exactly
the way `reflex run` would. Additional handlers can connect from external
processes via the SDK:

```go
client, _ := sdk.Connect(sdk.Remote("/tmp/reflex-demo.sock"))
h := sdk.NewHandler("my-handler",
    sdk.Consumes("MessageReceived"),
    sdk.Emits("ResponseEmitted"),
    sdk.Terminal("ResponseEmitted"),
).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
    return ctx.Emit("ResponseEmitted", sdk.Args{"text": "hi"})
})
client.Register(h)
client.Run(context.Background())   // blocks
```

Seed events arrive either from another connected client, or from the
existing CLI commands with `--daemon`:

```
reflex emit --type RequestReceived --payload '{"payload":"hi"}' \
    --daemon /tmp/reflex-demo.sock
```

Semantic guarantees: an SDK-registered handler is observationally
indistinguishable from a YAML-declared one. Same consumes/emits
declaration, same Terminal-painting (declared `Terminal` on the spec
auto-paints emitted events), same drain ordering. With Phase 4b the
remote transport is feature-complete vs in-process: it supports
projection RPCs, multi-handler per connection, post-drain quiescence
checks, and wait-predicates over the wire.

Wire-protocol kinds:

- `hello { version, handler{name, consumes, emits} }` (client→daemon).
  Multiple `hello`s per connection are allowed — each registers a fresh
  handler against the same socket (Phase 4b: multi-handler mux).
- `welcome { handler_name, version }` (daemon→client). One per accepted
  `hello`.
- `deliver { delivery_id, handler_name, event }` (daemon→client). The
  `handler_name` field is the demultiplex key when one client hosts N
  handlers on one connection.
- `emit { delivery_id?, event }` (client→daemon — with `delivery_id` the
  emit is buffered onto the in-flight delivery; without one it's a fresh
  seed).
- `ack { delivery_id }` / `nack { delivery_id, error }` (client→daemon).
- `goodbye` (either side).
- `error { error }` (daemon→client, fatal-for-connection).

Phase 4b additions:

- `await { await_id, predicate, request_id }` (client→daemon). Registers
  a wait predicate (`drain` / `request_id_terminal` / `projection.has=<k>`)
  the daemon will resolve after every drain.
- `resolved { await_id, predicate, request_id }` (daemon→client). One per
  satisfied predicate. Sent at most once per await_id; the entry is
  dropped from the daemon's pending list on resolution.
- `timeout { await_id, reason }` (daemon→client; reserved — not yet
  emitted by the daemon, the CLI's own deadline does the cancellation).
- `proj_get { rpc_id, request_id, key }` (client→daemon). Looks up the
  projection store on behalf of a remote handler.
- `proj_value { rpc_id, request_id, key, value, found }` (daemon→client).
  The response to `proj_get`. `value` is the JSON-encoded payload;
  `found` reports whether the key existed.
- `proj_set { request_id, key, value }` (client→daemon). Fire-and-forget;
  the daemon writes into its projection store.

Anti-goal: this is still not a new sync primitive. Wait-predicates over
the socket are async on the wire — `await` is a notification subscription
and `resolved` is the notification when the predicate first becomes true.
The CLI blocks; the daemon does not. Multiple in-flight `await`s from
multiple CLI clients are evaluated lazily after each drain step.

The daemon now also runs `CheckQuiescence` after every successful drain
(when wired in via `daemon.SetQuiescence`), so `RequestUnhandled` and
`EventOrphaned` surface for socket-driven emits identically to
in-process `reflex run`.

What 4b still leaves to 4c/4d/4e: permission checks on register,
scaffold CLI for new handlers, the external HTTP / Go-embed embedder API.
